package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/moby/go-archive"
	"github.com/moby/moby/v2/integration-cli/cli"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

type fileType uint32

const (
	ftRegular fileType = iota
	ftDir
	ftSymlink
)

type fileData struct {
	filetype fileType
	path     string
	contents string
	uid      int
	gid      int
	mode     int
}

func (fd fileData) creationCommand() string {
	var command string

	switch fd.filetype {
	case ftRegular:
		// Don't overwrite the file if it already exists!
		command = fmt.Sprintf("if [ ! -f %s ]; then echo %q > %s; fi", fd.path, fd.contents, fd.path)
	case ftDir:
		command = fmt.Sprintf("mkdir -p %s", fd.path)
	case ftSymlink:
		command = fmt.Sprintf("ln -fs %s %s", fd.contents, fd.path)
	}

	return command
}

func mkFilesCommand(fds []fileData) string {
	commands := make([]string, len(fds))

	for i, fd := range fds {
		commands[i] = fd.creationCommand()
	}

	return strings.Join(commands, " && ")
}

var defaultFileData = []fileData{
	{ftRegular, "file1", "file1", 0, 0, 0o666},
	{ftRegular, "file2", "file2", 0, 0, 0o666},
	{ftRegular, "file3", "file3", 0, 0, 0o666},
	{ftRegular, "file4", "file4", 0, 0, 0o666},
	{ftRegular, "file5", "file5", 0, 0, 0o666},
	{ftRegular, "file6", "file6", 0, 0, 0o666},
	{ftRegular, "file7", "file7", 0, 0, 0o666},
	{ftDir, "dir1", "", 0, 0, 0o777},
	{ftRegular, "dir1/file1-1", "file1-1", 0, 0, 0o666},
	{ftRegular, "dir1/file1-2", "file1-2", 0, 0, 0o666},
	{ftDir, "dir2", "", 0, 0, 0o666},
	{ftRegular, "dir2/file2-1", "file2-1", 0, 0, 0o666},
	{ftRegular, "dir2/file2-2", "file2-2", 0, 0, 0o666},
	{ftDir, "dir3", "", 0, 0, 0o666},
	{ftRegular, "dir3/file3-1", "file3-1", 0, 0, 0o666},
	{ftRegular, "dir3/file3-2", "file3-2", 0, 0, 0o666},
	{ftDir, "dir4", "", 0, 0, 0o666},
	{ftRegular, "dir4/file3-1", "file4-1", 0, 0, 0o666},
	{ftRegular, "dir4/file3-2", "file4-2", 0, 0, 0o666},
	{ftDir, "dir5", "", 0, 0, 0o666},
	{ftSymlink, "symlinkToFile1", "file1", 0, 0, 0o666},
	{ftSymlink, "symlinkToDir1", "dir1", 0, 0, 0o666},
	{ftSymlink, "brokenSymlinkToFileX", "fileX", 0, 0, 0o666},
	{ftSymlink, "brokenSymlinkToDirX", "dirX", 0, 0, 0o666},
	{ftSymlink, "symlinkToAbsDir", "/root", 0, 0, 0o666},
	{ftDir, "permdirtest", "", 2, 2, 0o700},
	{ftRegular, "permdirtest/permtest", "perm_test", 65534, 65534, 0o400},
}

func defaultMkContentCommand() string {
	return mkFilesCommand(defaultFileData)
}

func makeTestContentInDir(t *testing.T, dir string) {
	t.Helper()
	for _, fd := range defaultFileData {
		path := filepath.Join(dir, filepath.FromSlash(fd.path))
		switch fd.filetype {
		case ftRegular:
			assert.NilError(t, os.WriteFile(path, []byte(fd.contents+"\n"), os.FileMode(fd.mode)))
		case ftDir:
			assert.NilError(t, os.Mkdir(path, os.FileMode(fd.mode)))
		case ftSymlink:
			assert.NilError(t, os.Symlink(fd.contents, path))
		}

		if fd.filetype != ftSymlink && runtime.GOOS != "windows" {
			assert.NilError(t, os.Chown(path, fd.uid, fd.gid))
		}
	}
}

type testContainerOptions struct {
	addContent bool
	readOnly   bool
	volumes    []string
	workDir    string
	command    string
}

func makeTestContainer(t *testing.T, options testContainerOptions) (containerID string) {
	t.Helper()
	if options.addContent {
		mkContentCmd := defaultMkContentCommand()
		if options.command == "" {
			options.command = mkContentCmd
		} else {
			options.command = fmt.Sprintf("%s && %s", defaultMkContentCommand(), options.command)
		}
	}

	if options.command == "" {
		options.command = "#(nop)"
	}

	args := []string{"run", "-d"}

	for _, volume := range options.volumes {
		args = append(args, "-v", volume)
	}

	if options.workDir != "" {
		args = append(args, "-w", options.workDir)
	}

	if options.readOnly {
		args = append(args, "--read-only")
	}

	args = append(args, "busybox", "/bin/sh", "-c", options.command)

	out := cli.DockerCmd(t, args...).Combined()

	containerID = strings.TrimSpace(out)

	out = cli.DockerCmd(t, "wait", containerID).Combined()

	exitCode := strings.TrimSpace(out)
	if exitCode != "0" {
		out = cli.DockerCmd(t, "logs", containerID).Combined()
	}
	assert.Equal(t, exitCode, "0", "failed to make test container: %s", out)

	return containerID
}

func makeCatFileCommand(path string) string {
	return fmt.Sprintf("if [ -f %s ]; then cat %s; fi", path, path)
}

func cpPath(pathElements ...string) string {
	localizedPathElements := make([]string, len(pathElements))
	for i, path := range pathElements {
		localizedPathElements[i] = filepath.FromSlash(path)
	}
	return strings.Join(localizedPathElements, string(filepath.Separator))
}

func cpPathTrailingSep(pathElements ...string) string {
	return fmt.Sprintf("%s%c", cpPath(pathElements...), filepath.Separator)
}

func containerCpPath(containerID string, pathElements ...string) string {
	joined := strings.Join(pathElements, "/")
	return fmt.Sprintf("%s:%s", containerID, joined)
}

func containerCpPathTrailingSep(containerID string, pathElements ...string) string {
	return fmt.Sprintf("%s/", containerCpPath(containerID, pathElements...))
}

func runDockerCp(t *testing.T, src, dst string) error {
	t.Helper()

	args := []string{"cp", src, dst}
	if out, _, err := runCommandWithOutput(exec.Command(dockerBinary, args...)); err != nil {
		return fmt.Errorf("error executing `docker cp` command: %s: %s", err, out)
	}
	return nil
}

func startContainerGetOutput(t *testing.T, containerID string) (string, error) {
	t.Helper()

	args := []string{"start", "-a", containerID}

	out, _, err := runCommandWithOutput(exec.Command(dockerBinary, args...))
	if err != nil {
		return "", fmt.Errorf("error executing `docker start` command: %s: %s", err, out)
	}

	return out, nil
}

func getTestDir(t *testing.T, label string) (tmpDir string) {
	t.Helper()
	var err error

	tmpDir, err = os.MkdirTemp("", label)
	// unable to make temporary directory
	assert.NilError(t, err)

	return tmpDir
}

func isCpDirNotExist(err error) is.Comparison {
	return is.ErrorContains(err, archive.ErrDirNotExists.Error())
}

func isCpCannotCopyDir(err error) is.Comparison {
	return is.ErrorContains(err, archive.ErrCannotCopyDir.Error())
}

func fileContentEquals(t *testing.T, filename, contents string) error {
	t.Helper()

	fileBytes, err := os.ReadFile(filename)
	if err != nil {
		return err
	}

	expectedBytes, err := io.ReadAll(strings.NewReader(contents))
	if err != nil {
		return err
	}

	if !bytes.Equal(fileBytes, expectedBytes) {
		return fmt.Errorf("file content not equal - expected %q, got %q", string(expectedBytes), string(fileBytes))
	}

	return nil
}

func symlinkTargetEquals(t *testing.T, symlink, expectedTarget string) error {
	t.Helper()

	actualTarget, err := os.Readlink(symlink)
	if err != nil {
		return err
	}

	if actualTarget != expectedTarget {
		return fmt.Errorf("symlink target points to %q not %q", actualTarget, expectedTarget)
	}

	return nil
}

func containerStartOutputEquals(t *testing.T, containerID, contents string) error {
	t.Helper()

	out, err := startContainerGetOutput(t, containerID)
	if err != nil {
		return err
	}

	if out != contents {
		return fmt.Errorf("output contents not equal - expected %q, got %q", contents, out)
	}

	return nil
}

func defaultVolumes(tmpDir string) []string {
	if testEnv.IsLocalDaemon() {
		return []string{
			"/vol1",
			fmt.Sprintf("%s:/vol2", tmpDir),
			fmt.Sprintf("%s:/vol3", filepath.Join(tmpDir, "vol3")),
			fmt.Sprintf("%s:/vol_ro:ro", filepath.Join(tmpDir, "vol_ro")),
		}
	}

	// Can't bind-mount volumes with separate host daemon.
	return []string{"/vol1", "/vol2", "/vol3", "/vol_ro:/vol_ro:ro"}
}
