//go:build windows

package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Run the real PowerShell template with test-owned native executables. The
// curl.exe fixture supplies bytes without networking; the product fixture logs
// invocations without modifying an installed Paperboat service or account.
func TestNativeWindowsInstallerProductPinsAndEnrollmentContinuity(t *testing.T) {
	shell := filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	root := t.TempDir()
	fixture := filepath.Join(root, "fixture.exe")
	compile := filepath.Join(root, "compile.ps1")
	code := `using System;
using System.IO;
using System.Reflection;
public class InstallerFixture {
 public static int Main(string[] args) {
  string current=Assembly.GetExecutingAssembly().Location;
  if (Path.GetFileName(current).Equals("curl.exe",StringComparison.OrdinalIgnoreCase)) {
   string output=null; for(int i=0;i<args.Length-1;i++) if(args[i]=="--output") output=args[i+1];
   if(output==null) return 2;
   File.AppendAllText(Environment.GetEnvironmentVariable("PB_FIXTURE_DOWNLOADS"),args[args.Length-1]+"\t"+output+"\n");
   byte[] bytes=File.ReadAllBytes(Environment.GetEnvironmentVariable("PB_FIXTURE_PRODUCT"));
   string mode=Environment.GetEnvironmentVariable("PB_FIXTURE_MODE");
   if(mode=="truncated") Array.Resize(ref bytes,bytes.Length-1);
   if(mode=="corrupt") bytes[bytes.Length-1]^=1;
   File.WriteAllBytes(output,bytes);return 0;
  }
  File.AppendAllText(Environment.GetEnvironmentVariable("PB_FIXTURE_INVOCATIONS"),current+"\t"+String.Join("\t",args)+"\n");
  string installed=Environment.GetEnvironmentVariable("PB_FIXTURE_INSTALLED");
  string path=installed.Replace("\\","\\\\");
  if(args.Length>0 && args[0]=="install") {
   if(Environment.GetEnvironmentVariable("PB_FIXTURE_MODE")=="install-failure") return 17;
   File.Copy(current,installed,true);
   Console.WriteLine("{\"ok\":true,\"data\":{\"executable\":\""+path+"\"}}");return 0;
  }
  if(args.Length>0 && args[0]=="reset") {
   string resume=Environment.GetEnvironmentVariable("PB_FIXTURE_MODE")=="resume"?"true":"false";
   Console.WriteLine("{\"ok\":true,\"data\":{\"resume\":"+resume+",\"executable\":\""+(resume=="true"?path:"")+"\"}}");return 0;
  }
  if(args.Length>0 && args[0]=="pair") {
   int i=Array.IndexOf(args,"--enrollment-token-file");
   if(i<0 || i+1>=args.Length || !File.Exists(args[i+1])) return 3;
   return 0;
  }
  if(args.Length==1 && args[0]=="--version") {Console.WriteLine("fixture version");return 0;}
  return 4;
 }
}`
	compileBody := "$ErrorActionPreference='Stop'\nAdd-Type -OutputAssembly '" + strings.ReplaceAll(fixture, "'", "''") + "' -OutputType ConsoleApplication -TypeDefinition @'\n" + code + "\n'@\n"
	if err := os.WriteFile(compile, []byte(compileBody), 0600); err != nil {
		t.Fatal(err)
	}
	compileCtx, cancelCompile := context.WithTimeout(context.Background(), time.Minute)
	defer cancelCompile()
	if output, err := exec.CommandContext(compileCtx, shell, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", compile).CombinedOutput(); err != nil {
		t.Fatalf("compile test-owned native fixture: %v\n%s", err, output)
	}
	bytes, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(bytes)
	template, err := os.ReadFile("install.ps1")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, mode, requested, wantFailure string
		pair                               bool
		wantOps                            []string
	}{
		{name: "direct-install", wantOps: []string{"install", "--version"}},
		{name: "retained-platform-version", requested: "2026.09.05.0", wantOps: []string{"install", "--version"}},
		{name: "wrong-digest", mode: "corrupt", wantFailure: "product digest mismatch"},
		{name: "truncated", mode: "truncated", wantFailure: "product length mismatch"},
		{name: "wrong-requested-version", requested: "2026.10.08.19", wantFailure: "Requested Paperboat version does not match"},
		{name: "pair", pair: true, wantOps: []string{"reset", "install", "pair"}},
		{name: "resume", mode: "resume", pair: true, wantOps: []string{"reset", "pair"}},
		{name: "install-failure", mode: "install-failure", wantFailure: "installation failed with exit code 17", wantOps: []string{"install"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			work := t.TempDir()
			bin := filepath.Join(work, "bin")
			if err := os.Mkdir(bin, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(bin, "curl.exe"), bytes, 0700); err != nil {
				t.Fatal(err)
			}
			installed := filepath.Join(work, "installed.exe")
			if err := os.WriteFile(installed, bytes, 0700); err != nil {
				t.Fatal(err)
			}
			enrollment := filepath.Join(work, "existing-enrollment")
			if err := os.WriteFile(enrollment, []byte("unchanged enrolled identity"), 0600); err != nil {
				t.Fatal(err)
			}
			temp := filepath.Join(work, "temp")
			if err := os.Mkdir(temp, 0700); err != nil {
				t.Fatal(err)
			}
			downloads := filepath.Join(work, "downloads")
			invocations := filepath.Join(work, "invocations")
			version := "2026.10.08.20"
			if test.name == "retained-platform-version" {
				version = "2026.09.05.0"
			}
			body := string(template)
			for _, arch := range []string{"amd64", "arm64"} {
				pinVersion := "2026.10.08.20"
				if test.name != "retained-platform-version" || arch == runtime.GOARCH {
					pinVersion = version
				}
				prefix := "@PAPERBOAT_PRODUCT_WINDOWS_" + strings.ToUpper(arch) + "_"
				values := map[string]string{"VERSION": pinVersion, "URL": "https://github.com/pinksaucepasta/paperboat-cli/releases/download/" + pinVersion + "/pb-windows-" + arch + ".exe", "SHA256": hex.EncodeToString(digest[:]), "LENGTH": fmt.Sprint(len(bytes))}
				for key, value := range values {
					body = strings.ReplaceAll(body, prefix+key+"@", value)
				}
			}
			script := filepath.Join(work, "install.ps1")
			if err := os.WriteFile(script, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, shell, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script)
			// Explicitly exclude real enrollment/repository overrides from the child.
			for _, entry := range os.Environ() {
				name := strings.ToUpper(strings.SplitN(entry, "=", 2)[0])
				if !strings.HasPrefix(name, "PAPERBOAT_") && name != "PATH" && name != "TEMP" && name != "TMP" {
					cmd.Env = append(cmd.Env, entry)
				}
			}
			cmd.Env = append(cmd.Env, "PATH="+bin+";"+os.Getenv("PATH"), "TEMP="+temp, "TMP="+temp, "PB_FIXTURE_PRODUCT="+fixture, "PB_FIXTURE_DOWNLOADS="+downloads, "PB_FIXTURE_INVOCATIONS="+invocations, "PB_FIXTURE_INSTALLED="+installed, "PB_FIXTURE_MODE="+test.mode, "PAPERBOAT_VERSION="+test.requested)
			if test.pair {
				cmd.Env = append(cmd.Env, "PAPERBOAT_ENROLLMENT_TOKEN=synthetic-fixture-token", "PAPERBOAT_MACHINE_ALIAS=fixture-machine")
			}
			output, runErr := cmd.CombinedOutput()
			if test.wantFailure == "" {
				if runErr != nil {
					t.Fatalf("installer: %v\n%s", runErr, output)
				}
			} else if runErr == nil || !strings.Contains(strings.ToLower(string(output)), strings.ToLower(test.wantFailure)) {
				t.Fatalf("failure boundary=%v output=%s", runErr, output)
			}
			calls, _ := os.ReadFile(invocations)
			var actual []string
			for _, line := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
				if line == "" {
					continue
				}
				parts := strings.Split(line, "\t")
				if len(parts) < 2 {
					t.Fatal("invalid fixture invocation")
				}
				actual = append(actual, parts[1])
				if parts[1] == "pair" && (parts[0] != installed || !strings.Contains(line, "--name\tfixture-machine") || !strings.Contains(line, "--server\thttps://api.pprbt.dev")) {
					t.Fatal("pairing did not use installed executable and existing options")
				}
			}
			if strings.Join(actual, ",") != strings.Join(test.wantOps, ",") {
				t.Fatalf("product invocations=%v want=%v", actual, test.wantOps)
			}
			downloadLog, _ := os.ReadFile(downloads)
			lines := strings.Split(strings.TrimSpace(string(downloadLog)), "\n")
			if test.name == "wrong-requested-version" {
				if len(downloadLog) != 0 {
					t.Fatal("version mismatch downloaded a product")
				}
			} else {
				if len(lines) != 1 || !strings.Contains(lines[0], "/releases/download/"+version+"/pb-windows-") {
					t.Fatalf("expected one immutable product download: %s", downloadLog)
				}
				parts := strings.Split(lines[0], "\t")
				if len(parts) != 2 {
					t.Fatal("invalid download fixture")
				}
				if _, err := os.Stat(filepath.Dir(parts[1])); !os.IsNotExist(err) {
					t.Fatal("installer retained its staged product or token directory")
				}
			}
			retained, err := os.ReadFile(enrollment)
			if err != nil || string(retained) != "unchanged enrolled identity" {
				t.Fatal("installer modified unrelated enrollment state")
			}
		})
	}
}
