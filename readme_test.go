package scalebacktest_test

import (
	"bytes"
	"flag"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"testing"

	scalebacktest "github.com/cb-Sigma/scale-backtest"
	"github.com/cb-Sigma/scale-backtest/internal/cli"
)

var update = flag.Bool("update", false, "rewrite the command output blocks in the documentation")

// A documentation block of the form
//
//	<!-- output: scale-backtest ARGS -->
//	```text
//	...
//	```
//
// must hold exactly what the command prints. Run `go test . -update` to
// refresh the blocks after a change in behaviour or output.
var block = regexp.MustCompile("(?s)<!-- output: scale-backtest ([^\n]*?) -->\n```text\n(.*?)```\n")

func TestDocumentedOutputIsReal(t *testing.T) {
	for _, path := range []string{"README.md", "docs/model.md", "docs/algorithms.md", "docs/trace-formats.md"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		matches := block.FindAllSubmatchIndex(data, -1)
		if path == "README.md" && len(matches) < 3 {
			t.Fatalf("README.md should have at least three checked output blocks, found %d", len(matches))
		}
		var out bytes.Buffer
		last := 0
		for _, m := range matches {
			args := strings.Fields(string(data[m[2]:m[3]]))
			var stdout, stderr bytes.Buffer
			if code := cli.Main(args, &stdout, &stderr); code == cli.ExitError {
				t.Fatalf("%s: `scale-backtest %s` failed: %s", path, strings.Join(args, " "), stderr.String())
			}
			got := stdout.String()
			if want := string(data[m[4]:m[5]]); got != want && !*update {
				t.Errorf("%s: the output block of `scale-backtest %s` is out of date; run `go test . -update`\n--- current output ---\n%s", path, strings.Join(args, " "), got)
			}
			out.Write(data[last:m[4]])
			out.WriteString(got)
			last = m[5]
		}
		out.Write(data[last:])
		if *update && !bytes.Equal(out.Bytes(), data) {
			if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// The examples are embedded for --demo; keep them small.
func TestEmbeddedExamplesAreSmall(t *testing.T) {
	var total int64
	err := fs.WalkDir(scalebacktest.Examples, "examples", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > 150<<10 {
			t.Errorf("%s is %d bytes; bundled samples must stay small", path, info.Size())
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if total > 300<<10 {
		t.Errorf("examples total %d bytes; keep them under 300 KiB", total)
	}
}
