package cloudinit

import (
	"strings"
	"testing"
)

const testKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest test@example"

func TestBuildEscapesShellCommandAndAddsKey(t *testing.T) {
	got, err := Build("echo 'hello: world' > /tmp/cps-test", testKey)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	for _, want := range []string{"#cloud-config", "ssh_pwauth: false", "ssh_authorized_keys:", "runcmd:", "hello: world"} {
		if !strings.Contains(s, want) {
			t.Fatalf("expected %q in user-data: %s", want, s)
		}
	}
	if strings.Contains(s, "password123") {
		t.Fatal("must not inject a hardcoded password")
	}
}

func TestBuildPassesThroughCloudConfigAndInjectsKey(t *testing.T) {
	got, err := Build("#cloud-config\npackages:\n  - curl\n", testKey)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if !strings.Contains(s, "packages:") || !strings.Contains(s, "ssh_authorized_keys:") || !strings.Contains(s, "ssh_pwauth: false") || !strings.Contains(s, testKey) {
		t.Fatalf("unexpected cloud-config: %s", s)
	}
}

func TestBuildAppendsKeyToExistingBlockList(t *testing.T) {
	got, err := Build("#cloud-config\nssh_authorized_keys:\n  - ssh-ed25519 AAAA old@example\npackages:\n  - curl\n", testKey)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if strings.Count(s, "ssh_authorized_keys:") != 1 || !strings.Contains(s, testKey) || !strings.Contains(s, "packages:") {
		t.Fatalf("unexpected merged cloud-config: %s", s)
	}
}

func TestBuildRejectsInlineSSHKeyList(t *testing.T) {
	_, err := Build("#cloud-config\nssh_authorized_keys: [\"ssh-ed25519 AAAA old@example\"]\n", testKey)
	if err == nil {
		t.Fatal("expected explicit error for inline key list")
	}
}
