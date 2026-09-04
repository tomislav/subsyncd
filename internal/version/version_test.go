package version

import "testing"

func TestDevelopmentVersionIsNonempty(t *testing.T) {
	if Value == "" {
		t.Fatal("version is empty")
	}
}
