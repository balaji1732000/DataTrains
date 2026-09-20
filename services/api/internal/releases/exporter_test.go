package releases

import "testing"

func TestNormalizedIDsAreSortedAndUnique(t *testing.T) {
	ids, err := normalizedIDs([]string{"sess_b", "sess_a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != "sess_a" || ids[1] != "sess_b" {
		t.Fatalf("ids = %v", ids)
	}
	if _, err := normalizedIDs([]string{"sess_a", "sess_a"}); err == nil {
		t.Fatal("duplicate IDs were accepted")
	}
}
