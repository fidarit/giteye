package diffvalidate

import "testing"

func TestValidateKeepsAbsoluteNewFileLines(t *testing.T) {
	diff := `diff --git a/a.go b/a.go
index 000..111 100644
--- a/a.go
+++ b/a.go
@@ -1,3 +1,4 @@
 package a
 const x = 1
+const y = 2
 func f() {}
@@ -20,2 +21,2 @@
-old()
+new()
`
	got := Validate(diff, []Comment{{Path: "a.go", Line: 3, Body: "valid"}, {Path: "a.go", Line: 200, Body: "stale"}, {Path: "other.go", Line: 1, Body: "wrong file"}})
	if len(got) != 1 || got[0].Body != "valid" {
		t.Fatalf("unexpected validated comments: %#v", got)
	}
	positions := GiteaComments(diff, got)
	if len(positions) != 1 || positions[0].Line != 3 {
		t.Fatalf("unexpected Gitea line: %#v", positions)
	}
	// Later hunks keep using absolute new-file line numbers.
	all := GiteaComments(diff, []Comment{{Path: "a.go", Line: 21, Body: "changed"}})
	if len(all) != 1 || all[0].Line != 21 {
		t.Fatalf("unexpected later hunk line: %#v", all)
	}
}
