package fusefs

import "testing"

func TestRecentTransfersBounds(t *testing.T) {
	st := &mountState{}

	for i := range recentCap + 10 {
		st.recordFinished(recentTransfer{Path: string(rune('a' + i%26))})
	}

	got := st.recentTransfers()
	if len(got) != recentCap {
		t.Fatalf("len = %d, want %d", len(got), recentCap)
	}
}

func TestRecentTransfersMostRecentFirst(t *testing.T) {
	st := &mountState{}

	st.recordFinished(recentTransfer{Path: "first"})
	st.recordFinished(recentTransfer{Path: "second"})
	st.recordFinished(recentTransfer{Path: "third"})

	got := st.recentTransfers()
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if got[0].Path != "third" || got[1].Path != "second" || got[2].Path != "first" {
		t.Fatalf("order = %+v, want [third second first]", got)
	}
}

func TestRecentTransfersEmpty(t *testing.T) {
	st := &mountState{}
	if got := st.recentTransfers(); len(got) != 0 {
		t.Fatalf("len = %d, want 0", len(got))
	}
}
