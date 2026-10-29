package internal

import (
	"bufio"
	"embed"
	"fmt"
	"regexp"
	"strings"

	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextv1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
)

//go:embed yaml/*
var yamls embed.FS
var scheme *runtime.Scheme
var codecFactory serializer.CodecFactory

func init() {
	scheme = runtime.NewScheme()
	_ = apiextv1.AddToScheme(scheme)
	_ = apiextv1beta1.AddToScheme(scheme)
	_ = clientgoscheme.AddToScheme(scheme)

	codecFactory = serializer.NewCodecFactory(scheme)
}

// DecodeYAML returns decoded Kubernetes YAML manifests from the internal/yaml
// directory. If asset cannot be found or if asset is invalid YAML, DecodeYAML
// panics.
func DecodeYAML(asset string) []kube.Object {
	raw, err := yamls.ReadFile(asset)
	if err != nil {
		panic(fmt.Errorf("could not load YAML asset '%s': %w", asset, err))
	}

	objs, err := decode(raw)
	if err != nil {
		panic(fmt.Errorf("could not decode YAML asset '%s': %w", asset, err))
	}

	return objs
}

func decode(data []byte) ([]kube.Object, error) {
	decode := codecFactory.UniversalDeserializer().Decode

	// NOTE: this could likely be implemented much more efficiently with
	// FindAllIndex or something of that nature.
	splitter := regexp.MustCompile("(^|\n)---($|\n)")

	i := 0
	spl := splitter.Split(string(data), -1)
	objects := make([]kube.Object, len(spl))

	for _, doc := range spl {
		// Skip blocks that are only composed of blank lines and/or comments.
		hasContent := false
		scanner := bufio.NewScanner(strings.NewReader(doc))
		for scanner.Scan() {
			s := strings.TrimSpace(scanner.Text())
			if s != "" && !strings.HasPrefix(s, "#") {
				hasContent = true
				break
			}
		}
		if !hasContent {
			continue
		}

		object, _, err := decode([]byte(doc), nil, nil)
		if err != nil {
			return nil, err
		}

		objects[i], err = kube.FromRuntime(object)
		if err != nil {
			return nil, err
		}
		i++
	}

	return objects[:i], nil
}
