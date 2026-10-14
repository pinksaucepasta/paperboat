package db

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

func TestValidateIsolatedTestDSN(t *testing.T) {
	valid := "postgres://user:secret@db.example.test:5432/paperboat_test?sslmode=disable"
	for name, raw := range map[string]string{
		"postgresql scheme": "postgresql://user@db.example.test/paperboat_TRK_test",
		"default port":      "postgres://user@db.example.test/paperboat_test",
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateIsolatedTestDSN(raw, ""); err != nil {
				t.Fatalf("valid DSN rejected: %v", err)
			}
		})
	}
	for name, raw := range map[string]string{
		"empty":                 "",
		"malformed":             "not a dsn",
		"wrong scheme":          "mysql://user@db.example.test/paperboat_test",
		"missing host":          "postgres:///paperboat_test",
		"missing database":      "postgres://user@db.example.test/",
		"non test database":     "postgres://user@db.example.test/paperboat",
		"test marker in middle": "postgres://user@db.example.test/paperboat_test_backup",
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateIsolatedTestDSN(raw, ""); err == nil {
				t.Fatalf("unsafe DSN accepted: %q", raw)
			}
		})
	}
	if err := ValidateIsolatedTestDSN(valid, "postgres://app@db.example.test:5432/paperboat"); err != nil {
		t.Fatalf("different database rejected: %v", err)
	}
	if err := ValidateIsolatedTestDSN(valid, "postgres://app@db.example.test:5432/paperboat_test"); err == nil {
		t.Fatal("application database target was accepted")
	}
	if err := ValidateIsolatedTestDSN(valid, "postgres://app@db.example.test/paperboat_test"); err == nil {
		t.Fatal("application database target with default port was accepted")
	}
	if err := ValidateIsolatedTestDSN(valid, "not a dsn"); err == nil {
		t.Fatal("malformed application DSN was accepted")
	}
}

func TestValidateIsolatedTestDSNDoesNotExposeMalformedCredential(t *testing.T) {
	const privatePassword = "synthetic-private-test-password"
	err := ValidateIsolatedTestDSN("postgres://tester:"+privatePassword+"%ZZ@db.example.test/paperboat_test", "")
	if err == nil || strings.Contains(err.Error(), privatePassword) {
		t.Fatal("malformed PostgreSQL URI error exposed credentials")
	}
	var parseErr *url.Error
	if !errors.As(err, &parseErr) {
		t.Fatal("malformed PostgreSQL URI did not preserve its parse cause")
	}
}
