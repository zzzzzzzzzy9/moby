package layer

import (
	"bytes"
	"io"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/moby/moby/v2/daemon/graphdriver"
	"github.com/moby/moby/v2/daemon/internal/stringid"
)

func tarFromFilesInGraph(graph graphdriver.Driver, graphID, parentID string, files ...FileApplier) ([]byte, error) {
	t, err := tarFromFiles(files...)
	if err != nil {
		return nil, err
	}

	if err := graph.Create(graphID, parentID, nil); err != nil {
		return nil, err
	}
	if _, err := graph.ApplyDiff(graphID, parentID, bytes.NewReader(t)); err != nil {
		return nil, err
	}

	ar, err := graph.Diff(graphID, parentID)
	if err != nil {
		return nil, err
	}
	defer ar.Close()

	return io.ReadAll(ar)
}

func TestLayerMigrationNoTarsplit(t *testing.T) {
	// TODO Windows: Figure out why this is failing
	if runtime.GOOS == "windows" {
		t.Skip("Failing on Windows")
	}
	tempDir := t.TempDir()

	layer1Files := []FileApplier{
		newTestFile("/root/.bashrc", []byte("# Boring configuration"), 0o644),
		newTestFile("/etc/profile", []byte("# Base configuration"), 0o644),
	}

	layer2Files := []FileApplier{
		newTestFile("/root/.bashrc", []byte("# Updated configuration"), 0o644),
	}

	graph, err := newVFSGraphDriver(filepath.Join(tempDir, "graphdriver-"))
	if err != nil {
		t.Fatal(err)
	}
	graphID1 := stringid.GenerateRandomID()
	graphID2 := stringid.GenerateRandomID()

	tar1, err := tarFromFilesInGraph(graph, graphID1, "", layer1Files...)
	if err != nil {
		t.Fatal(err)
	}

	tar2, err := tarFromFilesInGraph(graph, graphID2, graphID1, layer2Files...)
	if err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(tempDir, "layers")
	ls, err := newStoreFromGraphDriver(root, graph)
	if err != nil {
		t.Fatal(err)
	}

	newTarDataPath := filepath.Join(tempDir, ".migration-tardata")
	diffID, size, err := ls.(*layerStore).ChecksumForGraphID(graphID1, "", newTarDataPath)
	if err != nil {
		t.Fatal(err)
	}

	layer1a, err := ls.(*layerStore).RegisterByGraphID(graphID1, "", diffID, newTarDataPath, size)
	if err != nil {
		t.Fatal(err)
	}

	layer1b, err := ls.Register(bytes.NewReader(tar1), "")
	if err != nil {
		t.Fatal(err)
	}

	assertReferences(t, layer1a, layer1b)

	// Attempt register, should be same
	layer2a, err := ls.Register(bytes.NewReader(tar2), layer1a.ChainID())
	if err != nil {
		t.Fatal(err)
	}

	diffID, size, err = ls.(*layerStore).ChecksumForGraphID(graphID2, graphID1, newTarDataPath)
	if err != nil {
		t.Fatal(err)
	}

	layer2b, err := ls.(*layerStore).RegisterByGraphID(graphID2, layer1a.ChainID(), diffID, newTarDataPath, size)
	if err != nil {
		t.Fatal(err)
	}
	assertReferences(t, layer2a, layer2b)

	if metadata, err := ls.Release(layer2a); err != nil {
		t.Fatal(err)
	} else if len(metadata) > 0 {
		t.Fatalf("Unexpected layer removal after first release: %#v", metadata)
	}

	metadata, err := ls.Release(layer2b)
	if err != nil {
		t.Fatal(err)
	}

	assertMetadata(t, metadata, createMetadata(layer2a))
}
