package backend

import "testing"

func TestWorkerCountLargeFilesAndGitHub(t *testing.T) {
	cases := []struct {
		name   string
		total  int64
		github bool
		want   int
	}{
		{"small generic", 4 << 20, false, 1},
		{"28MB generic", 28 << 20, false, 14},
		{"100MB generic", 100 << 20, false, 16},
		{"1GB generic", 1 << 30, false, 16},
		{"4GB generic", 4 << 30, false, 16},
		{"28MB github capped", 28 << 20, true, 4},
		{"1GB github full", 1 << 30, true, 16},
		{"2GB github full", 2 << 30, true, 16},
		{"threshold below github capped", (256 << 20) - 1, true, 4},
		{"threshold at github full", 256 << 20, true, 16},
	}
	for _, c := range cases {
		if got := workerCount(c.total, c.github); got != c.want {
			t.Errorf("%s: workerCount=%d want %d", c.name, got, c.want)
		}
	}
}

// The old implementation created exactly workerCount chunks, so a 3GB file
// had 16 x ~193MB tails and one slow connection could dominate the whole
// download. rangedChunks must produce many small work units instead.
func TestRangedChunksAreSmallAndCoverFile(t *testing.T) {
	cases := []struct {
		name    string
		total   int64
		workers int
		minN    int
		maxSize int64
	}{
		{"10MB", 10 << 20, 5, 5, 2 << 20},
		{"28MB", 28 << 20, 14, 14, 2 << 20},
		{"100MB", 100 << 20, 16, 32, 16 << 20},
		{"1GB", 1 << 30, 16, 64, 16 << 20},
		{"3GB", 3 << 30, 16, 192, 16 << 20},
		{"28MB github 4 workers", 28 << 20, 4, 8, 16 << 20},
	}
	for _, c := range cases {
		chunks := rangedChunks(c.total, c.workers)
		if len(chunks) < c.minN {
			t.Errorf("%s: got %d chunks, want >= %d", c.name, len(chunks), c.minN)
		}
		var sum int64
		for _, ch := range chunks {
			size := ch.Size()
			if size <= 0 {
				t.Errorf("%s: non-positive chunk %+v", c.name, ch)
			}
			if size > c.maxSize {
				t.Errorf("%s: chunk size %d exceeds %d", c.name, size, c.maxSize)
			}
			if ch.Done != 0 {
				t.Errorf("%s: fresh chunk has Done=%d", c.name, ch.Done)
			}
			sum += size
		}
		if sum != c.total {
			t.Errorf("%s: chunks cover %d, want %d", c.name, sum, c.total)
		}
		// Consecutive ranges must be contiguous and start at 0.
		var off int64
		for i, ch := range chunks {
			if ch.Start != off || ch.End < ch.Start {
				t.Errorf("%s: chunk %d has bad range %+v (want start %d)", c.name, i, ch, off)
			}
			off = ch.End + 1
		}
	}
}
