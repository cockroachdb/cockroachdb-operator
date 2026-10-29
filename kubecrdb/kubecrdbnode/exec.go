package kubecrdbnode

//go:generate mockgen -package=kubecrdbnode -destination=./mocks.go . Executor,NodeExecutor

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/manifests"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"github.com/cockroachlabs/version"
	"go.uber.org/zap"
)

const (
	// the path to the cockroach binary in the container
	cockroachBinary             = "/cockroach/cockroach"
	decommissionHeaderRowPrefix = "id,is_live,replicas"
	alreadyDecommissionedSuffix = "it is not allowed to rejoin the cluster"
)

// A timed-out command can make client-go return while it is still copying
// output in another goroutine. synchronizedBuffer keeps that final write from
// racing with exec reading the partial output for its error.
type synchronizedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *synchronizedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

// NodeExecutor defines an interface used to run commands on the node. This is
// intended for use in testing and will always be a kubecrdbnode.NodeExecutor
// in production.
type NodeExecutor interface {
	Init(context.Context) (string, string, error)

	NodeID(context.Context) (int64, error)

	// StoreIDs returns the CRDB store IDs owned by the given node ID, read from
	// crdb_internal.kv_store_status. They are used to reclaim range-shared Basalt
	// directories after decommissioning.
	StoreIDs(context.Context, int64) ([]int64, error)

	// Replicas returns the number of replicas stored on the node.
	Replicas(context.Context, uint) (uint64, error)
	QueryRegionStatus(context.Context, crdbv1beta1.CrdbClusterRegion) error

	// Drain is a blocking call to drain SQL connections and leases.
	Drain(context.Context, uint) (string, error)

	// DryRunDecommission is a blocking call that will run a dry
	// run decommission for all provided node ids.
	DryRunDecommission(context.Context, []uint) (string, string, error)

	// Decommission is a non blocking call that will initiate the decommission
	// procedure. Decommission should only be called after Drain() has
	// succeeded.
	Decommission(context.Context, uint) error

	// FinalizeDecommission permanently removes the node from cluster
	// membership. Finalization is required because we run `cockroach
	// decommission` with the `--wait none` flag.
	FinalizeDecommission(context.Context, uint) error

	// IsDecommissioning returns true if the node is currently decommissioning
	IsDecommissioning(context.Context, uint) (bool, error)

	// IsFullyDecommissioned returns true if the node has reached terminal
	// membership=decommissioned in the CRDB cluster.
	IsFullyDecommissioned(context.Context, uint) (bool, error)

	// Kill forcibly kills the cockroach process running on the container.
	// This is useful to workaround cases where the cockroach process hangs
	// during termination.
	// https://github.com/cockroachdb/cockroach/issues/111620
	Kill(context.Context) error

	// ReplicasDeprecated returns the number of replicas stored on the
	// node.
	ReplicasDeprecated(context.Context, uint) (uint64, error)
	// FinalizeDecommission permanently removes the node from cluster
	// membership. Finalization is required because we run `cockroach
	// decommission` with the `--wait none` flag.
	FinalizeDecommissionDeprecated(context.Context, uint) error

	// Version returns the version of the cockroach binary running on the node.
	Version(context.Context) (string, error)

	// UnderReplicatedRanges returns the cluster-wide count of under-replicated
	// ranges. The check runs against this node's pod but reports a cluster-wide
	// value, so any running node can answer it.
	UnderReplicatedRanges(context.Context) (int64, error)

	// ClusterHealth returns the cluster-wide count of under-replicated ranges
	// and the number of live nodes.
	ClusterHealth(context.Context) (underReplicated int64, liveNodes int32, err error)
}

// Executor defines an interface used to run commands on a node. The values for
// the pod name and container are fixed and will be the node's pod name and the
// container name defined by manifests.CRDBContainerName.
//
// This interface is satisfied by kube.Ctl.
type Executor interface {
	Exec(context.Context, string, string, kube.ExecOptions) error
}

// NewNodeExecutor creates a new NodeExecutor for the specified node.
func NewNodeExecutor(node *crdbv1beta1.CrdbNode, exec Executor) NodeExecutor {
	return &nodeExecutor{node: node, executor: exec}
}

// nodeExecutor is used to run commands on crdb node pods.
type nodeExecutor struct {
	node     *crdbv1beta1.CrdbNode
	executor Executor
}

// Drain issues the drain command so the node starts shedding connections
// and leases. Note that this command is blocking.
func (n *nodeExecutor) Drain(ctx context.Context, nodeID uint) (string, error) {
	stdout, _, err := n.exec(ctx, []string{
		cockroachBinary,
		"node",
		"drain",
		strconv.Itoa(int(nodeID)),
		"--port",
		fmt.Sprintf("%d", n.node.Spec.GetGRPCPort()),
	})

	return stdout, err
}

// FinalizeDecommissionDeprecated finalizes the decommissioning process by executing cockroach node
// decommission (again). This should only be called after Decommission has been called
// and Replicas returns 0. This step is final and cannot be undone.
func (n *nodeExecutor) FinalizeDecommissionDeprecated(ctx context.Context, nodeID uint) error {
	decommissioned, err := n.isDecommissioned(ctx, nodeID)
	if err != nil {
		return err
	}
	if decommissioned {
		return nil
	}
	_, _, err = n.exec(ctx, []string{
		cockroachBinary,
		"node",
		"decommission",
		strconv.Itoa(int(nodeID)),
		"--port",
		fmt.Sprintf("%d", n.node.Spec.GetGRPCPort()),
	})

	return err
}

// FinalizeDecommission finalizes the decommissioning process by executing
// `cockroach node decommission` (again). This should only be called after
// Decommission has been called once and Replicas returns 0.  Finalization is
// required because we run `cockroach decommission` with the `--wait none`
// flag. This step is final and cannot be undone.
func (n *nodeExecutor) FinalizeDecommission(ctx context.Context, nodeID uint) error {
	details, err := n.getDecommissionDetails(ctx, nodeID)
	if err != nil {
		return err
	}

	if details.membership == "decommissioned" {
		return nil
	}

	_, _, err = n.exec(ctx, []string{
		cockroachBinary,
		"node",
		"decommission",
		strconv.Itoa(int(nodeID)),
		"--port",
		fmt.Sprintf("%d", n.node.Spec.GetGRPCPort()),
	})

	return err
}

func (n *nodeExecutor) DryRunDecommission(
	ctx context.Context, nodeIDs []uint,
) (string, string, error) {
	nodeIDStrs := make([]string, len(nodeIDs))
	for i, nodeID := range nodeIDs {
		nodeIDStrs[i] = strconv.Itoa(int(nodeID))
	}

	cmd := []string{
		cockroachBinary,
		"node",
		"decommission",
		"--dry-run",
		"--port",
		fmt.Sprintf("%d", n.node.Spec.GetGRPCPort()),
	}
	cmd = append(cmd, nodeIDStrs...)

	return n.exec(ctx, cmd)
}

// Decommission starts the decommissioning process by executing cockroach node
// decommission. This will not wait for the node to be decommissioned. Once the
// node has finished rehoming all its ranges, a call to FinalizeDecommission
// must be issued to finalize the process.
func (n *nodeExecutor) Decommission(ctx context.Context, nodeID uint) error {
	stdout, stderr, err := n.exec(ctx, []string{
		cockroachBinary,
		"node",
		"decommission",
		fmt.Sprintf("%d", nodeID),
		"--wait",
		"none",
		"--port",
		fmt.Sprintf("%d", n.node.Spec.GetGRPCPort()),
	})
	if stderr != "" && stderr != "<nil>" {
		logging.Warnf(ctx, "CRDB decomission returned non-empty stderr: %s", stderr)
	}
	// Same tolerance as getDecommissionDetails, for a much narrower
	// needle-in-haystack window. If the operator crashes between this
	// exec and the UpdateStatus that would have advanced us to
	// TransferringReplicas, the next reconcile lands here again from the
	// Drained state.
	//
	// By then CRDB may have already finished the transfer and severed
	// the node, in which case this exec exits with the "permanently
	// removed" marker. Swallowing that error lets the state machine
	// advance instead of looping in Drained forever.
	if err != nil && isNodeAlreadyDecommissionedError(nodeID, stdout) {
		return nil
	}
	return err
}

// Init runs cockroach init to initialize the cluster.
func (n *nodeExecutor) Init(ctx context.Context) (string, string, error) {
	args := []string{
		cockroachBinary,
		"init",
		"--host",
		fmt.Sprintf(":%d", n.node.Spec.GetGRPCPort()),
	}

	if n.node.Spec.VirtualCluster != nil {
		switch n.node.Spec.VirtualCluster.Mode {
		case crdbv1beta1.VirtualClusterModeNone:
			// disabled mode runs a plain init with no virtualization flags
		case crdbv1beta1.VirtualClusterModePrimary:
			args = append(args, "--virtualized")
		case crdbv1beta1.VirtualClusterModeStandby:
			args = append(args, "--virtualized-empty")
		default:
			return "", "", errors.Newf("unknown virtual cluster mode: %q", n.node.Spec.VirtualCluster.Mode)
		}
	}

	return n.exec(ctx, args)
}

// QueryRegionStatus queries the status of the nodes in the provided
// region from the node. This is a way to determine if there are nodes
// in another region that have been initialized.
func (n *nodeExecutor) QueryRegionStatus(
	ctx context.Context, region crdbv1beta1.CrdbClusterRegion,
) error {
	cluster := n.node.Labels[manifests.BetaCrdbClusterNodeOwnerLabelKey]
	domain := region.Domain
	if domain == "" {
		domain = "cluster.local"
	}

	regionNamespace := region.Code
	if region.Namespace != "" {
		regionNamespace = region.Namespace
	}

	// Using the join service here ensures that we'll only hit ready nodes and
	// remove the possibility of false positives. In theory, we could probably
	// just perform a DNS look up and look for any valid addresses as this is a
	// headless service.
	//
	// COCKROACH_INSECURE in the container env does not apply here because
	// --url overrides it, so sslmode must be set in the URL directly.
	sslMode := "verify-ca"
	if !n.node.Spec.TLSEnabled {
		sslMode = "disable"
	}
	url := fmt.Sprintf("postgresql://%s?sslmode=%s", manifests.JoinFQDN(cluster, regionNamespace, domain), sslMode)

	stdout, stderr, err := n.exec(ctx, []string{
		cockroachBinary,
		"node",
		"status",
		"--url",
		url,
	})
	logging.Infof(
		ctx,
		"executed node status query",
		zap.String("stdout", stdout),
		zap.String("stderr", stderr),
	)

	return err
}

// NodeID returns the CRDB node id for this node. It does this by fetching the
// node status and parsing the results accordingly.
func (n *nodeExecutor) NodeID(ctx context.Context) (int64, error) {
	stdout, _, err := n.exec(ctx, []string{
		cockroachBinary,
		"node",
		"status",
		"--decommission",
		"--format",
		"csv",
	})
	if err != nil {
		return 0, err
	}

	r := csv.NewReader(strings.NewReader(stdout))
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}

		if err != nil {
			return 0, errors.Wrap(err, "reading node record")
		}

		id, host := rec[0], rec[1]
		prefix := fmt.Sprintf(
			"%s.%s.%s",
			n.node.Name,
			n.node.Labels[manifests.BetaCrdbClusterNodeOwnerLabelKey],
			n.node.Namespace,
		)
		if strings.HasPrefix(host, prefix) {
			nid, err := strconv.ParseInt(id, 10, 64)
			return nid, errors.Wrapf(err, "parsing id for node: %s", n.node.Name)
		}
	}

	return 0, errors.Newf("node not found: %s", n.node.Name)
}

// minVersionForUnsafeInternals is the first CockroachDB version that restricts
// access to crdb_internal tables and requires allow_unsafe_internals. StoreIDs
// is currently the only NodeExecutor query that needs this session setting.
var minVersionForUnsafeInternals = version.MustParse("v26.1.0")

// storeIDColumn is the column selected by the store ID query. Parsing looks for
// the exact header so output from the preceding SET statement cannot shift it.
const storeIDColumn = "store_id"

// StoreIDs returns the CRDB store IDs owned by nodeID, read from
// crdb_internal.kv_store_status through cockroach sql. No gRPC or CLI endpoint
// exposes the per-node store mapping, so SQL is the only way to capture it. The
// operator persists the result on CrdbNodeStatus while the node is healthy.
// after decommissioning, the node is no longer present in kv_store_status.
//
// Unlike ClusterHealth, which can run `cockroach node status --ranges` against
// any node for a cluster-wide signal, this query is scoped to one node_id and
// must run against that node's own Pod while it still serves SQL.
func (n *nodeExecutor) StoreIDs(ctx context.Context, nodeID int64) ([]int64, error) {
	cmd := []string{
		cockroachBinary,
		"sql",
		"--format=csv",
	}

	// CockroachDB 26.1 and later restrict crdb_internal by default. Older versions
	// do not recognize this session variable, so only send it for a known newer
	// image. If image parsing fails, skip the SET and let the query surface any
	// real access error.
	if v, err := n.node.CrdbVersion(); err == nil && v.AtLeast(minVersionForUnsafeInternals) {
		cmd = append(cmd, "--execute=SET allow_unsafe_internals = on")
	}

	query := fmt.Sprintf(
		"SELECT store_id FROM crdb_internal.kv_store_status WHERE node_id = %d ORDER BY store_id",
		nodeID,
	)
	cmd = append(cmd, fmt.Sprintf("--execute=%s", query))
	cmd = append(cmd, fmt.Sprintf("--port=%d", n.node.Spec.GetSQLPort()))
	// no --certs-dir or --insecure needed, the container env vars handle TLS mode

	stdout, stderr, err := n.exec(ctx, cmd)
	if err != nil {
		return nil, errors.Wrapf(err, "querying store ids for node %d, stderr: %s", nodeID, stderr)
	}

	return parseStoreIDs(stdout)
}

// parseStoreIDs scans for the exact store_id header because the optional SET
// statement writes its own output first.
//
// With SET:    "SET\nstore_id\n5\n6"
// Without SET: "store_id\n5\n6"
func parseStoreIDs(stdout string) ([]int64, error) {
	r := csv.NewReader(strings.NewReader(stdout))
	// The CockroachDB SQL CLI appends a "Time: 46ms" footer after the rows. It
	// does not match the header's field count, so allow variable-length records
	// and stop explicitly at the footer below.
	r.FieldsPerRecord = -1
	for {
		rec, err := r.Read()
		if err == io.EOF {
			return nil, errors.Newf("unexpected output, no %s header: %q", storeIDColumn, stdout)
		}
		if err != nil {
			return nil, errors.Wrap(err, "reading store id header")
		}
		if len(rec) == 1 && strings.TrimSpace(rec[0]) == storeIDColumn {
			break
		}
	}

	var ids []int64
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errors.Wrap(err, "reading store id row")
		}
		if len(rec) == 0 {
			continue
		}
		value := strings.TrimSpace(rec[0])
		if value == "" {
			continue
		}
		if strings.HasPrefix(value, "Time:") {
			break
		}
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return nil, errors.Wrapf(err, "parsing store id row %q", rec)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func (n *nodeExecutor) isDecommissioned(ctx context.Context, nodeID uint) (bool, error) {
	rec, err := n.decommissionStatus(ctx, nodeID)
	if err != nil {
		return false, err
	}
	return rec[11] == "decommissioned", nil
}

func (n *nodeExecutor) IsDecommissioning(ctx context.Context, nodeID uint) (bool, error) {
	details, err := n.getDecommissionDetails(ctx, nodeID)
	if err != nil {
		return false, err
	}
	return details.isDecommissioning, nil
}

func (n *nodeExecutor) IsFullyDecommissioned(ctx context.Context, nodeID uint) (bool, error) {
	details, err := n.getDecommissionDetails(ctx, nodeID)
	if err != nil {
		return false, err
	}
	return details.membership == "decommissioned", nil
}

// Replicas returns the number of replicas on the node.
func (n *nodeExecutor) Replicas(ctx context.Context, nodeID uint) (uint64, error) {
	details, err := n.getDecommissionDetails(ctx, nodeID)
	return details.replicas, err
}

// ReplicasDeprecated returns the number of replicas on the node. This is
// mainly used for the decommissioning process where we first
// decommission the node, then poll until this function returns 0
// replicas (and then call FinalizeDecommission).
func (n *nodeExecutor) ReplicasDeprecated(ctx context.Context, nodeID uint) (uint64, error) {
	rec, err := n.decommissionStatus(ctx, nodeID)
	if err != nil {
		return 0, err
	}

	if rec[11] == "decommissioned" {
		// Column 11 is `membership`.
		// The `node status --decommission` command actually sends a sql query against
		// internal tables to the cluster.
		// When a node is decommissioned, other nodes will reject the call, this
		// sometimes causes the decommissioned node show non-zero replicas, but is actually decommissioned.
		// In this case, we treat that this node is already decommissioned,
		// and hosts 0 live replicas.
		return 0, nil
	}

	// TODO(pseudomuto): Look into getting this value by name rather than ordinal.
	// As is this seems rather brittle and could lead to long debug sessions if the
	// ordinal changes between versions of cockroach.
	replicas, err := strconv.ParseUint(rec[9], 10, 64)
	return replicas, errors.Wrapf(err, "parsing number of replicas: %s (%d)", n.node.Name, nodeID)
}

func (n *nodeExecutor) Version(ctx context.Context) (string, error) {
	stdout, _, err := n.exec(ctx, []string{
		cockroachBinary,
		"version",
		"--build-tag",
	})
	return stdout, err
}

// UnderReplicatedRanges returns the cluster-wide count of under-replicated
// ranges. It drops the live node count that ClusterHealth also reports.
func (n *nodeExecutor) UnderReplicatedRanges(ctx context.Context) (int64, error) {
	underReplicated, _, err := n.ClusterHealth(ctx)
	return underReplicated, err
}

// ClusterHealth returns the cluster-wide count of under-replicated ranges and
// the number of live nodes, both read from `cockroach node status --ranges`.
//
// We don't query crdb_internal.kv_store_status ourselves though it's an alternative we could use.
// A direct query lands in whatever tenant the pod's SQL port connects to, so on a virtualized (UA)
// cluster it would have to explicitly target the system tenant to read physical KV health.
//
// One other reason to avoid querying through SQL is CockroachDB 26.1+ also keeps crdb_internal
// table behind SET allow_unsafe_internals (which might be restricted in future versions entirely).
// `node status` command handles both for us: it opens its own SQL client against the system tenant,
// and as first-party tooling it owns the internal-table access.
// We inherit the right tenant routing and version handling instead of tracking them ourselves.
//
// Under the hood the command still sums the ranges.underreplicated gauge from
// kv_store_status, but it groups by node and returns one row per node. Summing
// those rows gives the same cluster-wide total the old query produced, and it
// stays correct for multi-store nodes even though the operator runs single-store
// nodes today.
//
// This asks a different question than getDecommissionDetails, which reads a
// single node's replica count from `node decommission --dry-run` to watch that
// one node drain. Here we care about the whole cluster's replication and its
// live membership.
//
// The live node count is the number of is_live rows. We don't pass --all, so
// node status already leaves out nodes that are decommissioned and no longer
// live, while a node that's down or decommissioning still shows up reading
// is_live=false. So the count tracks the nodes that are actually up, which is
// what a membership floor check wants.
func (n *nodeExecutor) ClusterHealth(ctx context.Context) (int64, int32, error) {
	stdout, stderr, err := n.exec(ctx, []string{
		cockroachBinary,
		"node",
		"status",
		"--ranges",
		"--format",
		"csv",
		"--port",
		fmt.Sprintf("%d", n.node.Spec.GetSQLPort()),
	})
	if err != nil {
		return 0, 0, errors.Wrapf(err, "checking cluster health, stderr: %s", stderr)
	}

	return parseClusterHealth(stdout)
}

// parseClusterHealth pulls the range and liveness counts out of the node status
// CSV. The command runs under a TTY, so notices and stray blank lines can show up
// before the real output. We skip everything until we reach the row carrying both
// column headers, then look values up by header name so column order or extra
// columns from a newer cockroach don't shift the parse.
func parseClusterHealth(stdout string) (int64, int32, error) {
	const (
		underReplicatedColumn = "ranges_underreplicated"
		liveColumn            = "is_live"
	)

	reader := csv.NewReader(strings.NewReader(stdout))
	reader.FieldsPerRecord = -1

	// Scan past any leading notices until we hit the row that carries both
	// headers. We require both on the same row so a stray earlier line that
	// happens to contain one of the names can't pair up with the real header.
	underReplicatedIdx := -1
	liveIdx := -1
	for {
		record, err := reader.Read()
		if err == io.EOF {
			return 0, 0, errors.Newf(
				"unexpected node status output, missing %s or %s header: %q",
				underReplicatedColumn, liveColumn, stdout,
			)
		}
		if err != nil {
			return 0, 0, errors.Wrap(err, "parsing node status CSV")
		}

		underReplicatedIdx, liveIdx = -1, -1
		for i, column := range record {
			switch strings.TrimSpace(column) {
			case underReplicatedColumn:
				underReplicatedIdx = i
			case liveColumn:
				liveIdx = i
			}
		}
		if underReplicatedIdx >= 0 && liveIdx >= 0 {
			break
		}
	}

	var underReplicated int64
	var liveNodes int32
	rows := 0
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, 0, errors.Wrap(err, "parsing node status CSV")
		}

		if len(record) == 1 && strings.TrimSpace(record[0]) == "" {
			continue
		}
		// a short row means truncated or garbled output, so fail closed rather
		// than proceed on a partial count that could hide under-replicated ranges
		if underReplicatedIdx >= len(record) || liveIdx >= len(record) {
			return 0, 0, errors.Newf("unexpected node status CSV row: %q", record)
		}

		// node status left-joins the liveness list with per-node store status, so a
		// node that isn't reporting any store comes back with a NULL range count. A
		// node reads that way while it's down, draining, starting, or decommissioning.
		// It has no stores answering, so it holds zero under-replicated ranges of its
		// own. The ranges it used to carry surface as under-replicated on the live
		// nodes instead, so treating this row as zero keeps the cluster total right.
		var count int64
		if field := strings.TrimSpace(record[underReplicatedIdx]); !strings.EqualFold(field, "NULL") {
			parsed, err := strconv.ParseInt(field, 10, 64)
			if err != nil {
				return 0, 0, errors.Wrapf(err, "parsing under-replicated range count %q", field)
			}
			if parsed < 0 {
				return 0, 0, errors.Newf("under-replicated range count is negative: %d", parsed)
			}
			count = parsed
		}
		underReplicated += count

		live, err := strconv.ParseBool(strings.TrimSpace(record[liveIdx]))
		if err != nil {
			return 0, 0, errors.Wrapf(err, "parsing live node status %q", record[liveIdx])
		}
		if live {
			liveNodes++
		}
		rows++
	}

	if rows == 0 {
		return 0, 0, errors.New("node status returned no nodes")
	}

	return underReplicated, liveNodes, nil
}

type decommissionDetails struct {
	membership        string
	replicas          uint64
	isDecommissioning bool
}

func (n *nodeExecutor) getDecommissionDetails(
	ctx context.Context, nodeID uint,
) (decommissionDetails, error) {
	stdout, _, err := n.exec(ctx, []string{
		cockroachBinary,
		"node",
		"decommission",
		fmt.Sprintf("%d", nodeID),
		"--dry-run",
		"--format",
		"csv",
		"--port",
		fmt.Sprintf("%d", n.node.Spec.GetGRPCPort()),
	})

	// A fully-decommissioned node has its own process rejected by peers,
	// so this exec exits non-zero with the "permanently removed" marker
	// in stdout. Reading stdout before propagating the err keeps the
	// reconciler from missing the terminal state and looping forever.
	//
	// isDecommissioning=true here matches what the live CSV reports for
	// a decommissioned node. That lets callers in TransferringReplicas
	// advance to ZeroReplicas instead of resetting to Drained. See the
	// design comment in controllers/nodectrl/decommission.go for how
	// this fits with the ensurePod eviction skip.
	if isNodeAlreadyDecommissionedError(nodeID, stdout) {
		return decommissionDetails{"decommissioned", 0, true}, nil
	}

	if err != nil {
		return decommissionDetails{}, err
	}

	// The output in v23.1 looks like:
	//
	// `warning: node 2 is already decommissioning or decommissioned
	//
	// id,is_live,replicas,is_decommissioning,membership,is_draining,readiness,blocking_ranges
	// 2,true,0,true,decommissioning,true,ready,0`
	//
	// First strip warnings and whitespace.
	idx := strings.Index(stdout, decommissionHeaderRowPrefix)
	if idx < 0 {
		return decommissionDetails{}, errors.Newf("unexpected output: expected csv header prefix '%s'", decommissionHeaderRowPrefix)
	}
	stdout = stdout[idx:]

	membership, err := readCsvValue(stdout, "membership")
	if err != nil {
		return decommissionDetails{}, err
	}

	rawIsDecommissioning, err := readCsvValue(stdout, "is_decommissioning")
	if err != nil {
		return decommissionDetails{}, err
	}
	isDecommissioning := rawIsDecommissioning == "true"

	// When a node is decommissioned, other nodes will reject the call, this
	// sometimes causes the decommissioned node show non-zero replicas, but is actually decommissioned.
	// In this case, we treat that this node is already decommissioned,
	// and hosts 0 live replicas.
	if membership == "decommissioned" {
		return decommissionDetails{"decommissioned", 0, isDecommissioning}, nil
	}

	count, err := readCsvValue(stdout, "replicas")
	if err != nil {
		return decommissionDetails{membership, 0, isDecommissioning}, err
	}

	replicas, err := strconv.Atoi(count)
	if err != nil {
		return decommissionDetails{membership, 0, isDecommissioning},
			errors.Wrapf(err, "converting replicas string '%s' to int", count)
	}

	return decommissionDetails{membership, uint64(replicas), isDecommissioning}, nil
}

func (n *nodeExecutor) decommissionStatus(ctx context.Context, nodeID uint) ([]string, error) {
	stdout, _, err := n.exec(ctx, []string{
		cockroachBinary,
		"node",
		"status",
		fmt.Sprintf("%d", nodeID),
		"--decommission",
		"--format",
		"csv",
	})
	if err != nil {
		return nil, err
	}

	r := csv.NewReader(strings.NewReader(stdout))
	// Skip the header row
	if _, err := r.Read(); err != nil {
		return nil, errors.Wrap(err, "reading header row")
	}

	rec, err := r.Read()
	if err != nil {
		return nil, errors.Wrapf(err, "getting decommissioning status: %s (%d)", n.node.Name, nodeID)
	}
	return rec, nil
}

// Kill forcibly kills the cockroach process running on the container.
// This is useful to workaround cases where the cockroach process hangs
// during termination.
// https://github.com/cockroachdb/cockroach/issues/111620
func (n *nodeExecutor) Kill(ctx context.Context) error {
	_, _, err := n.exec(ctx, []string{
		"/bin/sh",
		"-cx",
		`pids=$(ls -l /proc/*/exe | awk -F/ "\$5 == \"cockroach\" {print \$3}"); if [ -n "$pids" ]; then kill -9 $pids; fi`,
	})

	if err != nil {
		errText := err.Error()
		// The Pod can outlive its container long enough for finalizer cleanup to
		// race containerd teardown. Both errors mean the process is already gone.
		if strings.Contains(errText, "container not found") ||
			(strings.Contains(errText, "task ") && strings.Contains(errText, " not found")) {
			return nil
		}
	}
	return err
}

// exec executes the given command in the cockroachdb container. It returns the
// stdout and stderr results of the command.
func (n *nodeExecutor) exec(ctx context.Context, cmd []string) (string, string, error) {
	logging.Infof(ctx, "executing from node: %s, command: %s", n.node.Name, strings.Join(cmd, " "))

	stdout := new(synchronizedBuffer)
	stderr := new(synchronizedBuffer)

	err := n.executor.Exec(ctx, n.node.Name, manifests.CRDBContainerName, kube.ExecOptions{
		Command: cmd,
		Stdout:  stdout,
		Stderr:  stderr,
		TTY:     true,
	})

	stdoutText := stdout.String()
	stderrText := stderr.String()
	// Ensure that stdout and stderr are included in the error, they'll have
	// much more information than just the error code.
	err = errors.Wrapf(err, "executing command: %s, stdout: %q, stderr: %q", cmd, stdoutText, stderrText)

	return stdoutText, stderrText, err
}

// Reads the first row value associated with the given column.
func readCsvValue(output, column string) (string, error) {
	r := csv.NewReader(strings.NewReader(output))

	header, err := r.Read()
	if err != nil {
		return "", errors.Wrap(err, "reading header row")
	}

	for i := range header {
		if header[i] == column {
			row, err := r.Read()
			if err != nil {
				return "", errors.Wrap(err, "reading first row")
			}
			return row[i], nil
		}
	}
	return "", errors.Newf("no column named %s in csv output", column)
}

// If the node is already decommissioned and removed from the
// cluster, it returns an error like:
//
// `nX was permanently removed from the cluster at <time>; it is not allowed to rejoin the cluster`
//
// This error message was introduced in v22.1.0, so it should be
// viable for every cluster currently in CC. Older clusters will
// get a reconciler error in place of the cleaner handling here.
//
// See: https://github.com/cockroachdb/cockroach/commit/02a991600d2688471cc3a1c415ac0e8e98b6a9a6
func isNodeAlreadyDecommissionedError(nodeID uint, stdout string) bool {
	return strings.Contains(
		stdout, fmt.Sprintf("n%d was permanently removed from the cluster", nodeID),
	) && strings.Contains(
		stdout, alreadyDecommissionedSuffix,
	)
}
