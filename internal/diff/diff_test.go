package diff

import "testing"

const sample = `diff --git a/main.go b/main.go
index 1111111..2222222 100644
--- a/main.go
+++ b/main.go
@@ -1,5 +1,6 @@
 package main

-import "fmt"
+import "log"
+import "os"

 func main() {
@@ -20,3 +21,4 @@ func helper() {
 	a := 1
 	b := 2
+	c := 3
 }
diff --git a/gone.txt b/gone.txt
deleted file mode 100644
--- a/gone.txt
+++ /dev/null
@@ -1,2 +0,0 @@
-bye
-now
diff --git a/img.png b/img.png
Binary files /dev/null and b/img.png differ
`

func TestParse(t *testing.T) {
	files := Parse([]byte(sample))
	if len(files) != 3 {
		t.Fatalf("got %d files", len(files))
	}
	f := files[0]
	if f.Path != "main.go" || len(f.Hunks) != 2 {
		t.Fatalf("file 0: %+v", f)
	}
	right := f.RightLines()
	for _, want := range []int{1, 2, 3, 4, 5, 6, 21, 22, 23, 24} {
		if !right[want] {
			t.Errorf("line %d missing from right side: %v", want, right)
		}
	}
	if right[7] || right[20] {
		t.Errorf("unexpected right lines: %v", right)
	}
	added, removed := f.Changes()
	if added != 3 || removed != 1 {
		t.Errorf("changes = %d/%d", added, removed)
	}
	if !files[1].Deleted || files[1].Path != "gone.txt" {
		t.Errorf("file 1: %+v", files[1])
	}
	if !files[2].Binary {
		t.Errorf("file 2 not binary")
	}
}

func TestIgnored(t *testing.T) {
	pats := []string{"*.lock", "vendor/", "*.min.js"}
	cases := map[string]bool{
		"Cargo.lock":            true,
		"a/b/yarn.lock":         true,
		"vendor/x/y.go":         true,
		"pkg/vendor/x.go":       true,
		"static/app.min.js":     true,
		"src/main.go":           false,
		"vendored/notreally.go": false,
	}
	for p, want := range cases {
		if got := Ignored(p, pats); got != want {
			t.Errorf("Ignored(%q) = %v, want %v", p, got, want)
		}
	}
}
