package backend

import "testing"

func TestChunkCountLargeFilesAndGitHub(t *testing.T) {
	cases := []struct {
		name   string
		total  int64
		github bool
		want   int
	}{
		{"small generic", 4 << 20, false, 1},
		{"28MB generic", 28 << 20, false, 14},
		{"1GB generic", 1 << 30, false, 16},
		{"4GB generic", 4 << 30, false, 16},
		{"28MB github capped", 28 << 20, true, 4},
		{"1GB github full", 1 << 30, true, 16},
		{"2GB github full", 2 << 30, true, 16},
		{"threshold below github capped", (256 << 20) - 1, true, 4},
		{"threshold at github full", 256 << 20, true, 16},
	}
	for _, c := range cases {
		if got := chunkCount(c.total, c.github); got != c.want {
			t.Errorf("%s: chunkCount=%d want %d", c.name, got, c.want)
		}
	}
}
