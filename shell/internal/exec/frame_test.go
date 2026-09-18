package exec

import (
	"bytes"
	"testing"
)

func TestOutboundFrames(t *testing.T) {
	if got, want := stdinFrame([]byte("ls\r")), []byte("\x00\x00ls\r"); !bytes.Equal(got, want) {
		t.Errorf("stdinFrame = %q, want %q", got, want)
	}
	if got, want := resizeFrame(120, 40), []byte("\x00\x04{\"Width\": 120, \"Height\": 40}"); !bytes.Equal(got, want) {
		t.Errorf("resizeFrame = %q, want %q", got, want)
	}
}

func TestDecode(t *testing.T) {
	tests := []struct {
		name    string
		message []byte
		kind    Kind
		payload string
		wantErr bool
	}{
		{"stdout", []byte("\x00\x01hello"), Stdout, "hello", false},
		{"stderr", []byte("\x00\x02oops"), Stderr, "oops", false},
		{"info has no stream byte", []byte("\x01Connecting..."), Info, "Connecting...", false},
		{"error has no stream byte", []byte("\x02no such container"), Error, "no such container", false},
		{"empty", nil, 0, "", true},
		{"forward without stream", []byte("\x00"), 0, "", true},
		{"unknown stream", []byte("\x00\x09x"), 0, "", true},
		{"unknown proxy byte", []byte("\x07x"), 0, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind, payload, err := decode(tt.message)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if kind != tt.kind || string(payload) != tt.payload {
				t.Errorf("decode = (%v, %q), want (%v, %q)", kind, payload, tt.kind, tt.payload)
			}
		})
	}
}

func TestEndpoint(t *testing.T) {
	const logStream = "https://eastus.azurecontainerapps.dev/subscriptions/sub/resourceGroups/rg/containerApps/api/revisions/api--1/replicas/r1/containers/api/logstream"

	t.Run("falls back to the log stream host", func(t *testing.T) {
		got, err := Endpoint("", logStream, "sub", "rg", "api", "api--1", "r1", "api", "/bin/sh -c ls")
		if err != nil {
			t.Fatal(err)
		}
		want := "wss://eastus.azurecontainerapps.dev/subscriptions/sub/resourceGroups/rg/containerApps/api/revisions/api--1/replicas/r1/containers/api/exec?command=%2Fbin%2Fsh+-c+ls"
		if got != want {
			t.Errorf("got  %s\nwant %s", got, want)
		}
	})

	t.Run("prefers the exec endpoint and keeps its query", func(t *testing.T) {
		got, err := Endpoint("https://proxy.example/exec/path?api-version=1", logStream, "sub", "rg", "api", "api--1", "r1", "api", "bash")
		if err != nil {
			t.Fatal(err)
		}
		if want := "wss://proxy.example/exec/path?api-version=1&command=bash"; got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	})

	t.Run("neither endpoint", func(t *testing.T) {
		if _, err := Endpoint("", "", "sub", "rg", "api", "api--1", "r1", "api", "sh"); err == nil {
			t.Error("want an error")
		}
	})
}
