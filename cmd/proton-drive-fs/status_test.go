package main

import (
	"testing"
	"time"

	"github.com/khaosdoctor/proton-drive-linux-fs/internal/state"
)

func TestStatusFromAPIOrFileFallsBackWithNoSocket(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	want := state.Status{Mountpoint: "/home/u/ProtonDrive", Version: "1.2.3", PID: 99, Updated: time.Now().Unix()}
	if err := state.WriteStatus(want); err != nil {
		t.Fatal(err)
	}

	st, fresh := statusFromAPIOrFile()
	if !fresh {
		t.Fatal("expected fresh=true from the status.json fallback")
	}
	if st.PID != want.PID || st.Version != want.Version {
		t.Fatalf("got %+v, want %+v", st, want)
	}
}

func TestStatusFromAPIOrFileNoSocketNoFile(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	if _, fresh := statusFromAPIOrFile(); fresh {
		t.Fatal("expected fresh=false with neither a socket nor a status file")
	}
}
