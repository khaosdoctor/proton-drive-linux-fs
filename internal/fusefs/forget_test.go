package fusefs

import (
	"testing"

	proton "github.com/henrybear327/go-proton-api"

	"github.com/khaosdoctor/proton-drive-linux-fs/internal/drive"
)

// TestForgottenDirsLeaveTheRegistry checks st.dirs shrinks back once the kernel forgets a
// directory or the folder is deleted remotely, instead of holding every directory ever walked.
func TestForgottenDirsLeaveTheRegistry(t *testing.T) {
	st := &mountState{dirs: make(map[string]*dirNode), parentOf: make(map[string]string)}
	root := &dirNode{st: st, node: node("root", "root")}
	a := &dirNode{st: st, node: node("a", "a")}
	b := &dirNode{st: st, node: node("b", "b")}
	for _, d := range []*dirNode{root, a, b} {
		st.register(d)
	}

	a.OnForget()
	if _, ok := st.dirs["a"]; ok || len(st.dirs) != 2 {
		t.Fatalf("dirs = %v, want a dropped after OnForget", st.dirs)
	}

	// A stale dirNode's forget must not drop the newer one a later lookup registered.
	newer := &dirNode{st: st, node: node("b", "b")}
	st.register(newer)
	b.OnForget()
	if st.dirs["b"] != newer {
		t.Fatalf("dirs[b] = %p, want the newer dirNode %p kept", st.dirs["b"], newer)
	}

	st.handle(drive.Event{Type: proton.LinkEventDelete, LinkID: "b", IsDir: true})
	if _, ok := st.dirs["b"]; ok || len(st.dirs) != 1 {
		t.Fatalf("dirs = %v, want b dropped after a remote delete", st.dirs)
	}
}
