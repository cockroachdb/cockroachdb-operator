# Standard labels required for Red Hat Marketplace certification
REDHAT_LABELS = {
    "name": "cockroachdb-operator",
    "vendor": "Cockroach Labs",
    "version": "{{STABLE_OPERATOR_VERSION}}",  # Semantic version from VERSION file
    "release": "{{STABLE_COMMIT_SHA}}",  # Will be stamped at build time
    "summary": "CockroachDB Operator",
    "description": "Kubernetes operator for managing CockroachDB clusters in Kubernetes environments",
    "io.k8s.display-name": "CockroachDB Operator",
    "io.k8s.description": "Kubernetes operator for managing CockroachDB clusters",
    "io.openshift.tags": "database,cockroachdb,sql",
    "com.redhat.component": "cockroachdb-operator",
    "maintainer": "Cockroach Labs <support@cockroachlabs.com>",
}

REDHAT_LABELS_INIT_CONTAINER = dict(REDHAT_LABELS, **{
    "name": "init-container",
    "summary": "CockroachDB Init Container",
    "description": "Initialization container for CockroachDB clusters handling locality and encryption setup",
    "com.redhat.component": "cockroachdb-init-container",
})

REDHAT_LABELS_INOTIFYWAIT = dict(REDHAT_LABELS, **{
    "name": "inotifywait",
    "summary": "CockroachDB Certificate Reloader",
    "description": "Sidecar container for detecting certificate changes and triggering reloads",
    "com.redhat.component": "cockroachdb-inotifywait",
})

def get_redhat_labels(component_type):
    """Get Red Hat labels for a specific component type.
    
    Args:
        component_type: One of "operator", "init-container", "inotifywait"
    
    Returns:
        Dictionary of label key-value pairs
    """
    if component_type == "operator":
        return REDHAT_LABELS
    elif component_type == "init-container":
        return REDHAT_LABELS_INIT_CONTAINER
    elif component_type == "inotifywait":
        return REDHAT_LABELS_INOTIFYWAIT
    else:
        fail("Unknown component type: %s" % component_type)
