{{/*
  Generates a CA and a signed cert for SQL connections (root
  cert).

  By convention, the first line is expected to be the ca.crt
  entry. Lines 2-3 are the client.root entries. Finally, lines
  4-5 are the client.node entries.

  TODO(pseudomuto): Should we make a configurable map of users
  here?
*/}}
{{- define "cluster.clientCerts" -}}
{{- $days := default .Values.certificates.validForDays 3650 | int -}}
{{- $ca := genCA "client-ca" 3650 -}}
{{- $cert := genSignedCert "root" nil nil $days $ca -}}
{{- $nodeCert := genSignedCert "node" nil nil $days $ca -}}
{{ $ca.Cert | b64enc }}
tls.crt: {{ $cert.Cert | b64enc }}
tls.key: {{ $cert.Key | b64enc }}
tls.crt: {{ $nodeCert.Cert | b64enc }}
tls.key: {{ $nodeCert.Key | b64enc }}
{{- end }}

{{/* 
  Generates a CA and a signed cert for inter-node communication (node 
  cert). For now, there is a single node cert used by all crdb nodes in
  the cluster.

  By convention, the first line is expected to be the ca.crt
  entry.
*/}}
{{- define "cluster.nodeCerts" -}}
{{/*
  The set of AltNames to add to the node cert. Right now this includes
  the internal discovery service and a wildcard for matching any crdb
  node in the cluster. Any values defined in 
  .Values.certificates.extraNodeAltNames will be included here.
*/}}
{{- $altNames := concat .Values.certificates.extraNodeAltNames (list
  (printf "%s" .Release.Name)
  (printf "%s.%s" .Release.Name .Release.Namespace)
  (printf "%s.%s.svc.cluster.local" .Release.Name .Release.Namespace)
  (printf "*.%s" .Release.Name)
  (printf "*.%s.%s" .Release.Name .Release.Namespace)
  (printf "*.%s.%s.svc.cluster.local" .Release.Name .Release.Namespace)
  "localhost"
) -}}
{{- $ips := list "127.0.0.1" -}}

{{- $days := default .Values.certificates.validForDays 3650 | int -}}
{{- $ca := genCA "node-ca" $days -}}
{{- $cert := genSignedCert "node" $ips $altNames $days $ca -}}
{{ $ca.Cert | b64enc }}
tls.crt: {{ $cert.Cert | b64enc }}
tls.key: {{ $cert.Key | b64enc }}
{{- end }}

{{/*
  Generates a self-signed cert for the UI. This is used by the DB console

  TODO: This is just dummy data right now. Will be updated as we stand up
  the CRDBNode objects.
*/}}
{{- define "cluster.httpCerts" -}}
{{- $days := default .Values.certificates.validForDays 3650 | int -}}
{{- $cert := genSelfSignedCert "example.com" (list "10.0.0.1" "10.0.0.2") (list "bar.com" "bat.com") $days -}}
tls.crt: {{ $cert.Cert | b64enc }}
tls.key: {{ $cert.Key | b64enc }}
{{- end }}

