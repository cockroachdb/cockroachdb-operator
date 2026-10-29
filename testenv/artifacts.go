package testenv

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/cockroachdb/errors"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kuberecord"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
)

// artifactDirName is the environment variable which gives the directory path
// where debugging artifacts (e.g. pod list, logs, etc) will be written when a
// test fails. When running in CI, those artifacts will get picked up by the
// CI system to make debugging the failures easier.
//
// If the ARTIFACT_DIR environment variable is empty, then no artifacts will be
// written. The directory can be an absolute path or a path that is relative to
// the working directory. Artifacts are written to a hierarchical sub-directory,
// according to each test or sub-test name. For example:
//
//	ARTIFACT_DIR=artifacts
//	  <working-dir>/artifacts/TestIntegrationCluster
//	  <working-dir>/artifacts/TestIntegrationCluster/TestCreate
//	  <working-dir>/artifacts/TestIntegrationCluster/TestCreateDeleteHost
//
//	ARTIFACT_DIR=/tmp/artifacts
//	  /tmp/artifacts/TestIt
const artifactDirName = "ARTIFACT_DIR"

// clearArtifacts removes the artifacts directory for this test. This should be
// called at the beginning of each test. It is a no-op if the ARTIFACT_DIR
// environment variable is empty.
func clearArtifacts(t *testing.T) {
	artifactDir := getArtifactDir(t)
	if artifactDir == "" {
		return
	}
	require.NoError(t, os.RemoveAll(artifactDir))
}

// writeArtifactsOnFailed creates a test cleanup function that will write
// debugging artifacts if the test fails. This can be called multiple times
// by the same test; once an artifact is written, it will not be written again.
// It can also be called in sub-tests, each of which gets its own sub-directory
// within the parent test's artifact directory.
func writeArtifactsOnFailed(t *testing.T) {
	// Don't write artifacts during kuberecord playback, since there's no K3D
	// cluster.
	if kuberecord.Mode() == kuberecord.ModeReplaying {
		return
	}

	t.Cleanup(func() {
		if !t.Failed() {
			// Test succeeded, so don't write anything.
			return
		}

		// Ensure the artifact directory for this test has been created.
		artifactDir := makeArtifactDir(t)
		if artifactDir == "" {
			// ARTIFACT_DIR is not set, so don't write anything.
			return
		}

		// Show all pods in all namespaces.
		runCmd(t, filepath.Join(artifactDir, "get-pods.txt"),
			"kubectl", "get", "pods", "-A", "-o", "wide", "--show-labels")

		// Cluster-wide events explain why pods never became Ready.
		runCmd(t, filepath.Join(artifactDir, "get-events.txt"),
			"kubectl", "get", "events", "-A", "--sort-by=.lastTimestamp")

		// Node conditions and taints explain why pods never got scheduled.
		runCmd(t, filepath.Join(artifactDir, "get-nodes.txt"),
			"kubectl", "get", "nodes", "-o", "wide", "--show-labels")
		runCmd(t, filepath.Join(artifactDir, "describe-nodes.txt"),
			"kubectl", "describe", "nodes")

		runCmd(t, filepath.Join(artifactDir, "crdbnodes.yaml"),
			"kubectl", "get", "crdbnodes", "-A", "-o", "yaml")

		writeK3DContainerLogs(t)

		// Write artifacts for the CRDB operator pods.
		writePodArtifacts(t, regexp.MustCompile("cockroach-operator"), "cockroach-operator")

		// Write artifacts for the sqlprober pods.
		writePodArtifacts(t, regexp.MustCompile("sqlprober"), "sqlprober")

		// Write artifacts for the SQL proxy pods.
		writePodArtifacts(t, regexp.MustCompile("sqlproxy"), "sqlproxy")
		writePodArtifacts(t, regexp.MustCompile("sqlproxy"), "tenantdir")

		// Write artifacts for all SQL pods in the RUNNING state.
		writePodArtifacts(t, regexp.MustCompile(`tenantpool.*state=RUNNING`), "cockroachdb")

		// Write artifacts for all KV pods.
		writePodArtifacts(t, regexp.MustCompile(`app=cockroachdb`), "cockroachdb")

		// Write artifacts for the Basalt integration test cluster.
		basaltPod := regexp.MustCompile(`app.kubernetes.io/name=` + regexp.QuoteMeta(basaltFixtureName))
		writePodArtifacts(t, basaltPod, "metadb")
		writePodArtifacts(t, basaltPod, "controller")
		for i := 1; i <= basaltFixtureBlobCount; i++ {
			writePodArtifacts(t, basaltPod, fmt.Sprintf("blob-%d", i))
		}
	})
}

// getArtifactDir returns the full path to the directory where this test's
// debugging artifacts should be stored. If the ARTIFACT_DIR environment
// variable is empty, then getArtifactDir returns the empty string. If
// ARTIFACT_DIR is relative, the returned path is relative to the current
// working directory. If the test is a sub-test, then its path contains a
// separate directory for it and each ancestor test.
func getArtifactDir(t *testing.T) string {
	artifactDir := os.Getenv(artifactDirName)
	if artifactDir == "" {
		return ""
	}
	artifactDir, err := filepath.Abs(artifactDir)
	require.NoError(t, err)
	return filepath.Join(artifactDir, t.Name())
}

// makeArtifactDir calls getArtifactDir and then ensures that all directories in
// the returned path have been created. It returns the full path of the artifact
// directory.
func makeArtifactDir(t *testing.T) string {
	artifactDir := getArtifactDir(t)
	if artifactDir == "" {
		return ""
	}

	err := os.MkdirAll(artifactDir, os.ModePerm)
	require.NoError(t, err)
	return artifactDir
}

// writePodArtifacts uses the "kubectl describe" and "kubectl logs" command to
// write artifacts for all K8s pods matched by the given regular expression (see
// findPodNames comment for details on matching). Only logs from the given
// container are written.
func writePodArtifacts(t *testing.T, match *regexp.Regexp, logContainer string) {
	artifactDir := getArtifactDir(t)
	podNames := findPodNames(t, match)
	for _, namespacedName := range podNames {
		// Run describe command.
		runCmd(t, filepath.Join(artifactDir, namespacedName.Name+".txt"),
			"kubectl", "describe", "pod",
			namespacedName.Name,
			"-n="+namespacedName.Namespace,
		)

		// Run logs command.
		runCmd(t, filepath.Join(artifactDir, fmt.Sprintf("%s-%s.log", namespacedName.Name, logContainer)),
			"kubectl", "logs",
			namespacedName.Name,
			"-n="+namespacedName.Namespace,
			"-c="+logContainer,
		)
	}
}

// writeK3DContainerLogs writes docker logs for every k3d node container on the
// host. A node whose k3s process died stops heartbeating and cannot be
// inspected through the K8s API; its docker log is the only record of why it
// died.
func writeK3DContainerLogs(t *testing.T) {
	artifactDir := getArtifactDir(t)
	buf := bytes.NewBuffer(nil)
	cmd := exec.Command("docker", "ps", "-a", "--filter", "label=app=k3d", "--format", "{{.Names}}")
	cmd.Stderr = buf
	cmd.Stdout = buf
	if err := cmd.Run(); err != nil {
		t.Logf("failed to list k3d containers: %v: %s", err, buf)
		return
	}

	for container := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		container = strings.TrimSpace(container)
		if container == "" {
			continue
		}
		runCmd(t, filepath.Join(artifactDir, "docker-"+container+".log"),
			"docker", "logs", container)
	}
}

// findPodNames returns the list of K8s pods matched by the given regular
// expression. The regex runs against custom output from this command:
//
//	kubectl get pods -A --no-headers=true --show-labels
//
// findPodNames returns the pod names qualified by pod namespace.
func findPodNames(t *testing.T, match *regexp.Regexp) []types.NamespacedName {
	buf := bytes.NewBuffer(nil)
	cmd := exec.Command("kubectl", "get", "pods", "-A", "--no-headers=true", "--show-labels")
	cmd.Stderr = buf
	cmd.Stdout = buf

	err := cmd.Run()
	require.NoError(t, err)

	var names []types.NamespacedName
	for line := range strings.SplitSeq(buf.String(), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || !match.MatchString(line) {
			continue
		}
		names = append(names, types.NamespacedName{Namespace: fields[0], Name: fields[1]})
	}

	return names
}

// runCmd runs the given command and pipes its output to a file of the given
// name. If the file already exists, runCmd is a no-op.
func runCmd(t *testing.T, outFile, cmdName string, args ...string) {
	if _, err := os.Stat(outFile); !errors.Is(err, os.ErrNotExist) {
		return
	}

	f, err := os.Create(outFile)
	require.NoError(t, err)

	cmd := exec.Command(cmdName, args...)
	cmd.Stderr = f
	cmd.Stdout = f

	// Ignore any errors - any error returned by the command should be piped to
	// the output file.
	_ = cmd.Run()
}
