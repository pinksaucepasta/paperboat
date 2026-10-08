module github.com/pinksaucepasta/paperboat

go 1.27.1

replace tailscale.com => github.com/pinksaucepasta/tailscale v1.103.0-pre.0.20260928002927-8c8b2e0dc313

require (
	github.com/google/uuid v1.6.0
	github.com/Microsoft/go-winio v0.6.2
	github.com/adrg/xdg v0.5.3
	github.com/anmitsu/go-shlex v0.0.0-20200514113438-38f4b401e2be
	github.com/bmatcuk/doublestar/v4 v4.10.0
	github.com/cenkalti/backoff/v5 v5.0.3
	github.com/charmbracelet/bubbles v0.21.0
	github.com/charmbracelet/bubbletea v1.3.10
	github.com/charmbracelet/lipgloss v1.1.0
	github.com/charmbracelet/x/ansi v0.11.8
	github.com/charmbracelet/x/term v0.2.2
	github.com/coder/websocket v1.8.15
	github.com/coreos/go-systemd/v22 v22.6.0
	github.com/creack/pty v1.1.24
	github.com/ebitengine/purego v0.10.2
	github.com/fsnotify/fsnotify v1.10.1
	github.com/fxamacker/cbor/v2 v2.9.3
	github.com/getsentry/sentry-go v0.49.0
	github.com/go-git/go-git/v5 v5.19.2
	github.com/godbus/dbus/v5 v5.2.2
	github.com/google/go-cmp v0.7.0
	github.com/google/renameio/v2 v2.0.2
	github.com/hashicorp/yamux v0.1.1
	github.com/hinshun/vt10x v0.0.0-20220301184237-5011da428d02
	github.com/junegunn/fzf v0.72.0
	github.com/klauspost/compress v1.20.0
	github.com/libp2p/go-yamux/v5 v5.1.0
	github.com/miekg/dns v1.1.73
	github.com/muesli/termenv v0.16.0
	github.com/pressly/goose/v3 v3.27.1
	github.com/quic-go/quic-go v0.61.0
	github.com/sethvargo/go-retry v0.3.0
	github.com/sigstore/sigstore v1.10.6
	github.com/spf13/cobra v1.10.2
	github.com/spf13/pflag v1.0.10
	github.com/tailscale/peercred v0.0.0-20250107143737-35a0c7bd7edc
	github.com/tailscale/squibble v0.0.0-20260411062017-141f5d618bc4
	github.com/tailscale/wireguard-go v0.0.0-20260911194433-e3222a3340cd
	github.com/theupdateframework/go-tuf/v2 v2.4.2
	github.com/zalando/go-keyring v0.2.8
	go.uber.org/goleak v1.3.0
	go4.org/mem v0.0.0-20240501181205-ae6ca9944745
	go4.org/netipx v0.0.0-20260823151212-3075585bcbeb
	golang.org/x/crypto v0.57.0
	golang.org/x/net v0.59.0
	golang.org/x/sys v0.48.0
	golang.org/x/term v0.46.0
	google.golang.org/grpc v1.80.0
	google.golang.org/protobuf v1.36.12
	howett.net/plist v1.0.1
	modernc.org/sqlite v1.49.1
	pgregory.net/rapid v1.3.0
	tailscale.com v1.103.0-pre.0.20260923014604-610b05c58e8d
)

require (
	9fans.net/go v0.0.8-0.20250307142834-96bdba94b63f // indirect
	dario.cat/mergo v1.0.2 // indirect
	filippo.io/edwards25519 v1.2.0 // indirect
	github.com/BurntSushi/toml v1.6.0
	github.com/ProtonMail/go-crypto v1.4.1 // indirect
	github.com/akutz/memconn v0.1.0 // indirect
	github.com/alexbrainman/sspi v0.0.0-20250919150558-7d374ff0d59e // indirect
	github.com/atotto/clipboard v0.1.4 // indirect
	github.com/aws/aws-sdk-go-v2 v1.46.0 // indirect
	github.com/aws/aws-sdk-go-v2/config v1.33.3 // indirect
	github.com/aws/aws-sdk-go-v2/credentials v1.20.3 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.19.2 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.5.2 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.8.2 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.5.2 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.19 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.14.2 // indirect
	github.com/aws/aws-sdk-go-v2/service/signin v1.9.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssm v1.77.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.37.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.42.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/sts v1.49.0 // indirect
	github.com/aws/smithy-go v1.28.1 // indirect
	github.com/axiomhq/hyperloglog v0.2.6 // indirect
	github.com/aymanbagabas/go-osc52/v2 v2.0.1 // indirect
	github.com/charmbracelet/colorprofile v0.4.3 // indirect
	github.com/charmbracelet/x/cellbuf v0.0.15 // indirect
	github.com/clipperhouse/displaywidth v0.11.0 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/cloudflare/circl v1.6.3 // indirect
	github.com/coreos/go-iptables v0.8.0 // indirect
	github.com/cpuguy83/go-md2man/v2 v2.0.7 // indirect
	github.com/creachadair/mds v0.31.0 // indirect
	github.com/creachadair/msync v0.10.1 // indirect
	github.com/cyphar/filepath-securejoin v0.7.0 // indirect
	github.com/danieljoos/wincred v1.2.3 // indirect
	github.com/dblohm7/wingoes v0.0.0-20260526185140-fb298caac7ca // indirect
	github.com/dgryski/go-metro v0.0.0-20250106013310-edb8663e5e33 // indirect
	github.com/digitalocean/go-smbios v0.0.0-20180907143718-390a4f403a8e // indirect
	github.com/djherbis/times v1.6.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/emirpasic/gods v1.18.1 // indirect
	github.com/erikgeiser/coninput v0.0.0-20211004153227-1c3628e74d0f // indirect
	github.com/gaissmai/bart v0.29.0 // indirect
	github.com/gitpod-io/xterm-go v0.0.0-20260907130418-dae5128cb6b3 // indirect
	github.com/go-git/gcfg v1.5.1-0.20230307220236-3a3c6141e376 // indirect
	github.com/go-git/go-billy/v5 v5.9.0 // indirect
	github.com/go-json-experiment/json v0.0.0-20260820222146-c27c302e5fc3 // indirect
	github.com/go-ole/go-ole v1.3.0 // indirect
	github.com/go4org/hashtriemap v0.0.0-20260824042624-45fcf11fca0e // indirect
	github.com/go4org/plan9netshell v0.0.0-20250324183649-788daa080737 // indirect
	github.com/golang/groupcache v0.0.0-20241129210726-2c02b8208cf8 // indirect
	github.com/google/btree v1.1.3 // indirect
	github.com/google/go-containerregistry v0.22.1 // indirect
	github.com/google/go-tpm v0.9.8 // indirect
	github.com/google/nftables v0.3.0 // indirect
	github.com/google/pprof v0.0.0-20250403155104-27863c87afa6 // indirect
	github.com/hdevalence/ed25519consensus v0.2.0 // indirect
	github.com/huin/goupnp v1.3.0 // indirect
	github.com/illarion/gonotify/v3 v3.0.2 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/insomniacslk/dhcp v0.0.0-20260901064844-234b97448fae // indirect
	github.com/jbenet/go-context v0.0.0-20150711004518-d14ea06fba99 // indirect
	github.com/jellydator/ttlcache/v3 v3.4.1 // indirect
	github.com/jsimonetti/rtnetlink v1.4.2 // indirect
	github.com/junegunn/go-shellwords v0.0.0-20250127100254-2aa3b3277741 // indirect
	github.com/kamstrup/intmap v0.5.2 // indirect
	github.com/kevinburke/ssh_config v1.2.0 // indirect
	github.com/klauspost/cpuid/v2 v2.3.0 // indirect
	github.com/kortschak/wol v0.0.0-20200729010619-da482cc4850a // indirect
	github.com/kr/fs v0.1.0 // indirect
	github.com/libp2p/go-buffer-pool v0.0.2 // indirect
	github.com/lucasb-eyer/go-colorful v1.4.1 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/mattn/go-localereader v0.0.1 // indirect
	github.com/mattn/go-runewidth v0.0.24 // indirect
	github.com/mdlayher/genetlink v1.4.0 // indirect
	github.com/mdlayher/netlink v1.11.2 // indirect
	github.com/mdlayher/sdnotify v1.0.0 // indirect
	github.com/mdlayher/socket v0.7.0 // indirect
	github.com/mfridman/interpolate v0.0.2 // indirect
	github.com/mitchellh/go-ps v1.0.0 // indirect
	github.com/muesli/ansi v0.0.0-20230316100256-276c6243b2f6 // indirect
	github.com/muesli/cancelreader v0.2.2 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/onsi/gomega v1.38.2 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/pierrec/lz4/v4 v4.1.26 // indirect
	github.com/pires/go-proxyproto v0.15.0 // indirect
	github.com/pjbgf/sha1cd v0.6.0 // indirect
	github.com/pkg/sftp v1.13.11 // indirect
	github.com/quic-go/qpack v0.6.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/russross/blackfriday/v2 v2.1.0 // indirect
	github.com/safchain/ethtool v0.7.0 // indirect
	github.com/secure-systems-lab/go-securesystemslib v0.11.0 // indirect
	github.com/sergi/go-diff v1.4.0 // indirect
	github.com/sigstore/protobuf-specs v0.5.0 // indirect
	github.com/skeema/knownhosts v1.3.1 // indirect
	github.com/tailscale/certstore v0.1.1-0.20260409135935-3638fb84b77d // indirect
	github.com/tailscale/gliderssh v0.3.4-0.20260716005906-1a0f895faf28 // indirect
	github.com/tailscale/go-winio v0.0.0-20231025203758-c4f33415bf55 // indirect
	github.com/tailscale/hujson v0.0.0-20260727124030-b80ff77dac4f // indirect
	github.com/tailscale/netlink v1.1.1-0.20240822203006-4d49adab4de7 // indirect
	github.com/tailscale/web-client-prebuilt v0.0.0-20260917222731-e0ed2d0d0fea // indirect
	github.com/tailscale/wf v0.0.0-20240214030419-6fbb0a674ee6 // indirect
	github.com/tailscale/xnet v0.0.0-20240729143630-8497ac4dab2e // indirect
	github.com/u-root/u-root v0.16.0 // indirect
	github.com/u-root/uio v0.0.0-20240224005618-d2acac8f3701 // indirect
	github.com/vishvananda/netns v0.0.5 // indirect
	github.com/wlynxg/anet v0.0.5 // indirect
	github.com/x448/float16 v0.8.4 // indirect
	github.com/xanzy/ssh-agent v0.3.3 // indirect
	github.com/xo/terminfo v0.0.0-20220910002029-abceb7e1c41e // indirect
	go.uber.org/multierr v1.11.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/exp v0.0.0-20260908205506-85c1c2202aba // indirect
	golang.org/x/exp/typeparams v0.0.0-20260811152304-ee035b5b010f // indirect
	golang.org/x/mod v0.41.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	golang.org/x/time v0.16.0 // indirect
	golang.org/x/tools v0.50.0 // indirect
	golang.zx2c4.com/wintun v0.0.0-20230126152724-0fa3db229ce2 // indirect
	golang.zx2c4.com/wireguard/windows v1.0.1 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260401024825-9d38bb4040a9 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260420184626-e10c466a9529 // indirect
	gopkg.in/warnings.v0 v0.1.2 // indirect
	gvisor.dev/gvisor v0.0.0-20260915211658-a6f909f08a72 // indirect
	honnef.co/go/tools v0.8.1 // indirect
	modernc.org/libc v1.74.1 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)
