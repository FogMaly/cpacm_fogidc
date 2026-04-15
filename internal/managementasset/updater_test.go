package managementasset

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEnsureLatestManagementHTML_PreservesPinnedNativeAsset(t *testing.T) {
	t.Helper()

	lastUpdateCheckMu.Lock()
	lastUpdateCheckTime = time.Time{}
	lastUpdateCheckMu.Unlock()

	staticDir := t.TempDir()
	localPath := filepath.Join(staticDir, managementAssetName)
	original := []byte(`<html><body><div id="root" data-cpapi-native-ai-providers="1"></div></body></html>`)
	if err := os.WriteFile(localPath, original, 0o644); err != nil {
		t.Fatalf("write local asset: %v", err)
	}

	if ok := EnsureLatestManagementHTML(context.Background(), staticDir, "", ""); !ok {
		t.Fatalf("EnsureLatestManagementHTML returned false")
	}

	got, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatalf("read local asset: %v", err)
	}
	if string(got) != string(original) {
		t.Fatalf("pinned native asset was modified\nwant: %s\ngot: %s", string(original), string(got))
	}
}
