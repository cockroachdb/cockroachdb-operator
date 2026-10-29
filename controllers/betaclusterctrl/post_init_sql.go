package betaclusterctrl

import (
	"context"
	"fmt"
	"strings"

	"github.com/cockroachdb/errors"
	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubenet"
	corev1 "k8s.io/api/core/v1"
)

// Statement is one Exec unit tagged with its source. Source is used in
// error messages so users can locate the failing input in their spec.
// A Statement's SQL may contain multiple semicolon-separated statements
// when the source is a ConfigMap or Secret blob; CockroachDB parses
// the string server-side over pgx's simple-query protocol.
type Statement struct {
	Source string
	SQL    string
}

// PostInitSQL collects and executes the SQL units declared in
// Spec.PostInitSQL. ResolveConfigMap and ResolveSecret read the referenced
// data; production callers wire these to the operator's kube client, while
// tests use in-memory fakes.
type PostInitSQL struct {
	Spec             *crdbv1beta1.PostInitSQL
	ResolveConfigMap func(*corev1.ConfigMapKeySelector) (string, error)
	ResolveSecret    func(*corev1.SecretKeySelector) (string, error)
}

// Collect returns the execution units in order: secret blob, configmap
// blob, then each inline list entry. Each source contributes at most
// one unit (the whole blob), except inline which contributes one unit
// per non-empty entry. Inline entries are trimmed; configmap and secret
// bodies are sent verbatim because CockroachDB handles their internal
// whitespace correctly.
func (p *PostInitSQL) Collect() ([]Statement, error) {
	if p.Spec == nil {
		return nil, nil
	}

	var out []Statement

	if ref := p.Spec.SecretRef; ref != nil {
		body, err := p.ResolveSecret(ref)
		if err != nil {
			return nil, errors.Wrapf(err, "reading secret %s/%s", ref.Name, ref.Key)
		}
		if strings.TrimSpace(body) != "" {
			out = append(out, Statement{
				Source: fmt.Sprintf("secret/%s/%s", ref.Name, ref.Key),
				SQL:    body,
			})
		}
	}

	if ref := p.Spec.ConfigMapRef; ref != nil {
		body, err := p.ResolveConfigMap(ref)
		if err != nil {
			return nil, errors.Wrapf(err, "reading configmap %s/%s", ref.Name, ref.Key)
		}
		if strings.TrimSpace(body) != "" {
			out = append(out, Statement{
				Source: fmt.Sprintf("configmap/%s/%s", ref.Name, ref.Key),
				SQL:    body,
			})
		}
	}

	for i, stmt := range p.Spec.Inline {
		trimmed := strings.TrimSpace(stmt)
		if trimmed == "" {
			continue
		}
		out = append(out, Statement{
			Source: fmt.Sprintf("inline[%d]", i),
			SQL:    trimmed,
		})
	}

	return out, nil
}

// Apply executes each collected unit sequentially using a connection
// with pgx's simple-query protocol enabled, which allows multi-statement
// blobs to be sent in a single ExecContext call. On the first failure
// it returns an error wrapped with the source label.
//
// Caller must set opts.PreferSimpleProtocol = true.
func (p *PostInitSQL) Apply(ctx context.Context, ctl *kube.Ctl, opts kubenet.Options) error {
	if !opts.PreferSimpleProtocol {
		return errors.New("apply requires opts.PreferSimpleProtocol = true")
	}

	stmts, err := p.Collect()
	if err != nil {
		return err
	}
	if len(stmts) == 0 {
		return nil
	}

	db, err := kubenet.OpenDB(ctx, ctl, opts)
	if err != nil {
		return errors.Wrap(err, "opening DB connection")
	}
	defer db.Close()

	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s.SQL); err != nil {
			return errors.Wrapf(err, "executing post-init SQL from %s", s.Source)
		}
	}
	return nil
}
