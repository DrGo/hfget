package hfget

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/drgo/hfget/testutils"
)

func TestResumeMetaMatches(t *testing.T) {
	assert := testutils.NewAssert(t)
	d := New("org/model", WithBranch("main"))
	file := HFFile{
		Path: "weights.bin",
		Size: 100,
		Oid:  "git-oid",
		LFS:  HFLFS{IsLFS: true, Oid: "lfs-sha"},
	}
	meta := d.newResumeMeta(file, resumeModeSingle, 0)
	assert.True(meta.matches(d, file, resumeModeSingle), "identical meta should match")

	other := file
	other.LFS.Oid = "other-sha"
	assert.False(meta.matches(d, other, resumeModeSingle), "different LFS oid must not match")

	other = file
	other.Size = 99
	assert.False(meta.matches(d, other, resumeModeSingle), "different size must not match")

	assert.False(meta.matches(d, file, resumeModeMulti), "different mode must not match")

	otherD := New("org/model", WithBranch("dev"))
	assert.False(meta.matches(otherD, file, resumeModeSingle), "different branch must not match")
}

func TestHasResumablePartial(t *testing.T) {
	require := testutils.NewRequire(t)
	assert := testutils.NewAssert(t)
	tmp := t.TempDir()
	d := New(mockRepoID, WithDestination(tmp))
	file := HFFile{Path: "a.bin", Size: 50, Oid: "o", LFS: HFLFS{Oid: "s"}}
	modelPath := d.getModelPath(mockRepoID)
	fullPath := filepath.Join(modelPath, file.Path)
	require.NoError(os.MkdirAll(filepath.Dir(fullPath), 0o755), "")

	assert.False(d.hasResumablePartial(modelPath, file), "no partial")

	require.NoError(os.WriteFile(singleTmpPath(fullPath), []byte("hello"), 0o644), "")
	require.NoError(writeResumeMeta(singleMetaPath(fullPath), d.newResumeMeta(file, resumeModeSingle, 0)), "")
	assert.True(d.hasResumablePartial(modelPath, file), "matching single partial")

	stale := d.newResumeMeta(file, resumeModeSingle, 0)
	stale.LFSOid = "nope"
	require.NoError(writeResumeMeta(singleMetaPath(fullPath), stale), "")
	assert.False(d.hasResumablePartial(modelPath, file), "stale single partial")
}
