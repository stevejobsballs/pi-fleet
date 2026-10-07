package setup

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// The double-click launchers in deploy/ run their helper scripts by paths
// relative to themselves ("$KIT/..."); every one must exist.
func TestDeployScriptReferencesExist(t *testing.T) {
	scripts, _ := filepath.Glob("../../deploy/*/*.sh")
	if len(scripts) == 0 {
		t.Fatal("no deploy scripts found")
	}
	ref := regexp.MustCompile(`"\$KIT/([^"$]+)"`)
	for _, s := range scripts {
		b, err := os.ReadFile(s)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range ref.FindAllStringSubmatch(string(b), -1) {
			if _, err := os.Stat(filepath.Join(filepath.Dir(s), m[1])); err != nil {
				t.Errorf("%s refers to %s, which doesn't exist", s, m[1])
			}
		}
	}
}
