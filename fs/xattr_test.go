package fs

import "testing"

func TestCombineSyncState(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                          string
		isDir, changes, session, data bool
		upload                        int
		want                          SyncState
	}{
		{"dir", true, true, true, false, uploadErrored, SyncLocal},
		{"cloud", false, false, false, false, 0, SyncCloud},
		{"local", false, false, false, true, 0, SyncLocal},
		{"modified", false, true, false, true, 0, SyncModified},
		{"queued", false, false, true, true, uploadNotStarted, SyncUploading},
		{"uploading", false, false, true, true, uploadStarted, SyncUploading},
		{"done", false, false, true, true, uploadComplete, SyncLocal},
		{"error", false, false, true, true, uploadErrored, SyncError},
		{"error beats modified", false, true, true, true, uploadErrored, SyncError},
		{"modified beats uploading", false, true, true, true, uploadStarted, SyncModified},
	}
	for _, tt := range tests {
		got := combineSyncState(tt.isDir, tt.changes, tt.session, tt.upload, tt.data)
		if got != tt.want {
			t.Errorf("%s: got %s, want %s", tt.name, got, tt.want)
		}
	}
}

func TestFillXAttr(t *testing.T) {
	t.Parallel()
	if n, st := fillXAttr([]byte("abc"), nil); n != 3 || st.Ok() {
		t.Errorf("size probe: got %d %v", n, st)
	}
	buf := make([]byte, 2)
	if n, st := fillXAttr([]byte("abc"), buf); n != 3 || st.Ok() {
		t.Errorf("small buffer: got %d %v", n, st)
	}
	buf = make([]byte, 8)
	if n, st := fillXAttr([]byte("abc"), buf); n != 3 || !st.Ok() || string(buf[:3]) != "abc" {
		t.Errorf("fit: got %d %v", n, st)
	}
}
