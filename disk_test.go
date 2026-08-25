package hfget

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/drgo/hfget/testutils"
)

func setSpaceLookupForTest(fn func(string) (int64, error)) func() {
	spaceLookupMu.Lock()
	orig := spaceLookup
	spaceLookup = fn
	spaceLookupMu.Unlock()
	return func() {
		spaceLookupMu.Lock()
		spaceLookup = orig
		spaceLookupMu.Unlock()
	}
}

func TestRequiredDownloadSpace(t *testing.T) {
	large := int64(20 * 1024 * 1024) // above 5*1MB multi-thread threshold
	cases := []struct {
		name     string
		plan     *DownloadPlan
		conns    int
		expected int64
	}{
		{name: "nil plan", expected: 0},
		{name: "empty plan", plan: &DownloadPlan{}, expected: 0},
		{
			name: "small files have no temp extra",
			plan: &DownloadPlan{
				TotalDownloadSize: 300,
				FilesToDownload: []FileDownload{
					{File: HFFile{Size: 100}},
					{File: HFFile{Size: 200}},
				},
			},
			conns:    5,
			expected: 300,
		},
		{
			name: "large LFS file adds merge extra",
			plan: &DownloadPlan{
				TotalDownloadSize: large + 50,
				FilesToDownload: []FileDownload{
					{File: HFFile{Size: large, LFS: HFLFS{IsLFS: true}}},
					{File: HFFile{Size: 50}},
				},
			},
			conns:    5,
			expected: large + 50 + large,
		},
		{
			name: "large non-LFS file does not add extra",
			plan: &DownloadPlan{
				TotalDownloadSize: large,
				FilesToDownload: []FileDownload{
					{File: HFFile{Size: large}},
				},
			},
			conns:    5,
			expected: large,
		},
		{
			name: "default connections when zero",
			plan: &DownloadPlan{
				TotalDownloadSize: large,
				FilesToDownload: []FileDownload{
					{File: HFFile{Size: large, LFS: HFLFS{IsLFS: true}}},
				},
			},
			conns:    0,
			expected: large + large,
		},
		{
			name: "overflow caps at MaxInt64",
			plan: &DownloadPlan{
				TotalDownloadSize: math.MaxInt64 - 10,
				FilesToDownload: []FileDownload{
					{File: HFFile{Size: math.MaxInt64, LFS: HFLFS{IsLFS: true}}},
				},
			},
			conns:    1,
			expected: math.MaxInt64,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert := testutils.NewAssert(t)
			got := requiredDownloadSpace(tc.plan, tc.conns)
			assert.True(got == tc.expected, "%s: expected %d, got %d", tc.name, tc.expected, got)
		})
	}
}

func TestExistingAncestor(t *testing.T) {
	require := testutils.NewRequire(t)
	assert := testutils.NewAssert(t)

	root := t.TempDir()
	missing := filepath.Join(root, "a", "b", "c")

	got := existingAncestor(missing)
	want, err := filepath.Abs(root)
	require.NoError(err, "")
	assert.True(got == want, "expected ancestor %s, got %s", want, got)

	absRoot, err := filepath.Abs(root)
	require.NoError(err, "")
	assert.True(existingAncestor(root) == absRoot, "existing path should resolve to itself, got %s", existingAncestor(root))
}

func TestAvailableSpaceTempDir(t *testing.T) {
	require := testutils.NewRequire(t)
	assert := testutils.NewAssert(t)

	avail, err := AvailableSpace(t.TempDir())
	require.NoError(err, "AvailableSpace on temp dir")
	assert.True(avail > 0, "expected positive free space, got %d", avail)
}

func TestEnsureWritableSpace(t *testing.T) {
	plan := &DownloadPlan{
		TotalDownloadSize: 1000,
		FilesToDownload:   []FileDownload{{File: HFFile{Path: "a.bin", Size: 1000}}},
	}

	t.Run("enough space", func(t *testing.T) {
		require := testutils.NewRequire(t)
		restore := setSpaceLookupForTest(func(string) (int64, error) { return 10_000, nil })
		defer restore()
		require.NoError(EnsureWritableSpace("/dest", plan, 5), "")
	})

	t.Run("not enough space", func(t *testing.T) {
		require := testutils.NewRequire(t)
		assert := testutils.NewAssert(t)
		restore := setSpaceLookupForTest(func(string) (int64, error) { return 10, nil })
		defer restore()
		err := EnsureWritableSpace("/dest", plan, 5)
		require.Error(err, "")
		assert.True(errors.Is(err, ErrInsufficientSpace), "expected ErrInsufficientSpace, got %v", err)
		assert.True(errors.Is(err, ErrInsufficientSpace) && err.Error() != ErrInsufficientSpace.Error(),
			"error should include path and sizes, got %v", err)
	})

	t.Run("lookup error does not block", func(t *testing.T) {
		require := testutils.NewRequire(t)
		restore := setSpaceLookupForTest(func(string) (int64, error) { return 0, errors.New("statfs failed") })
		defer restore()
		require.NoError(EnsureWritableSpace("/dest", plan, 5), "")
	})

	t.Run("zero download skips check", func(t *testing.T) {
		require := testutils.NewRequire(t)
		restore := setSpaceLookupForTest(func(string) (int64, error) {
			t.Fatal("space lookup should not run for an empty plan")
			return 0, nil
		})
		defer restore()
		require.NoError(EnsureWritableSpace("/dest", &DownloadPlan{}, 5), "")
	})
}

func TestBlocksToBytes(t *testing.T) {
	assert := testutils.NewAssert(t)
	assert.True(blocksToBytes(0, 4096) == 0, "zero blocks")
	assert.True(blocksToBytes(10, 0) == 0, "zero block size")
	assert.True(blocksToBytes(2, 4096) == 8192, "2*4096")
	assert.True(blocksToBytes(math.MaxUint64, 4096) == math.MaxInt64, "overflow should cap")
}

func TestExecutePlanInsufficientSpace(t *testing.T) {
	require := testutils.NewRequire(t)
	assert := testutils.NewAssert(t)

	restore := setSpaceLookupForTest(func(string) (int64, error) { return 1, nil })
	defer restore()

	tmpDir := t.TempDir()
	d := New(mockRepoID, WithDestination(tmpDir))
	plan := &DownloadPlan{
		Repo:              &RepoInfo{ID: mockRepoID},
		TotalDownloadSize: 1024,
		FilesToDownload:   []FileDownload{{File: HFFile{Path: "file.bin", Size: 1024}}},
	}

	err := d.ExecutePlan(context.Background(), plan)
	require.Error(err, "expected insufficient space error")
	assert.True(errors.Is(err, ErrInsufficientSpace), "expected ErrInsufficientSpace, got %v", err)

	_, statErr := os.Stat(filepath.Join(d.getModelPath(mockRepoID), "file.bin"))
	assert.True(os.IsNotExist(statErr), "file should not be downloaded when space check fails")
}
