package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFixture(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadPressure(t *testing.T) {
	content := "some avg10=12.34 avg60=5.67 avg300=1.00 total=123456\n" +
		"full avg10=8.90 avg60=2.00 avg300=0.50 total=98765\n"
	path := writeFixture(t, content)

	got, err := readPressure(path)
	if err != nil {
		t.Fatalf("readPressure() error = %v", err)
	}
	if got != 12.34 {
		t.Errorf("readPressure() = %v, want 12.34", got)
	}
}

func TestReadPressure_Zero(t *testing.T) {
	content := "some avg10=0.00 avg60=0.00 avg300=0.00 total=0\n" +
		"full avg10=0.00 avg60=0.00 avg300=0.00 total=0\n"
	path := writeFixture(t, content)

	got, err := readPressure(path)
	if err != nil {
		t.Fatalf("readPressure() error = %v", err)
	}
	if got != 0 {
		t.Errorf("readPressure() = %v, want 0", got)
	}
}

func TestReadPressure_MissingFile(t *testing.T) {
	if _, err := readPressure(filepath.Join(t.TempDir(), "nonexistent")); err == nil {
		t.Fatal("expected error for a nonexistent path, got nil")
	}
}

func TestReadPressure_MalformedContent(t *testing.T) {
	path := writeFixture(t, "not what /proc/pressure/memory looks like\n")
	if _, err := readPressure(path); err == nil {
		t.Fatal("expected error for malformed content, got nil")
	}
}

func TestDefaultGateway(t *testing.T) {
	// A real /proc/net/route sample for a guest at 169.254.100.2/24 whose
	// default gateway is 169.254.100.1: fields are tab-separated, address
	// fields are 4-byte hex in the kernel's little-endian printed order
	// (see hexLEToIPv4's own doc comment). "0164FEA9" decodes, byte by
	// byte reversed, to A9.FE.64.01 = 169.254.100.1.
	content := "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT\n" +
		"eth0\t0064FEA9\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n" +
		"eth0\t00000000\t0164FEA9\t0003\t0\t0\t0\t00000000\t0\t0\t0\n"
	path := writeFixture(t, content)

	gw, err := defaultGateway(path)
	if err != nil {
		t.Fatalf("defaultGateway() error = %v", err)
	}
	if got, want := gw.String(), "169.254.100.1"; got != want {
		t.Errorf("defaultGateway() = %s, want %s", got, want)
	}
}

func TestDefaultGateway_NoDefaultRoute(t *testing.T) {
	content := "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT\n" +
		"eth0\t0064FEA9\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n"
	path := writeFixture(t, content)

	if _, err := defaultGateway(path); err == nil {
		t.Fatal("expected error when no default route is present, got nil")
	}
}

func TestDefaultGateway_MissingFile(t *testing.T) {
	if _, err := defaultGateway(filepath.Join(t.TempDir(), "nonexistent")); err == nil {
		t.Fatal("expected error for a nonexistent path, got nil")
	}
}

func TestHexLEToIPv4(t *testing.T) {
	cases := []struct {
		hex  string
		want string
	}{
		{"0164FEA9", "169.254.100.1"},
		{"0100007F", "127.0.0.1"},
		{"00000000", "0.0.0.0"},
	}
	for _, c := range cases {
		got, err := hexLEToIPv4(c.hex)
		if err != nil {
			t.Errorf("hexLEToIPv4(%q) error = %v", c.hex, err)
			continue
		}
		if got.String() != c.want {
			t.Errorf("hexLEToIPv4(%q) = %s, want %s", c.hex, got, c.want)
		}
	}
}

func TestHexLEToIPv4_InvalidInput(t *testing.T) {
	if _, err := hexLEToIPv4("not-hex"); err == nil {
		t.Fatal("expected error for non-hex input, got nil")
	}
	if _, err := hexLEToIPv4("AABB"); err == nil {
		t.Fatal("expected error for too-short input, got nil")
	}
}
