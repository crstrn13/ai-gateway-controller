package boundary

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"regexp"
	"strings"

	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
)

// ParseManifests reads a (possibly multi-document) YAML manifest file into a slice of
// decoded documents. Empty documents are skipped.
func ParseManifests(path string) ([]map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := k8syaml.NewYAMLOrJSONDecoder(bufio.NewReader(bytes.NewReader(raw)), 4096)
	var docs []map[string]any
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		if len(doc) > 0 {
			docs = append(docs, doc)
		}
	}
	return docs, nil
}

// DeploymentContainerArgs returns the first container's args for the named Deployment.
func DeploymentContainerArgs(docs []map[string]any, name string) ([]string, bool) {
	for _, doc := range docs {
		if str(doc["kind"]) != "Deployment" || str(dig(doc, "metadata", "name")) != name {
			continue
		}
		rawArgs, ok := dig(doc, "spec", "template", "spec", "containers", 0, "args").([]any)
		if !ok {
			return nil, false
		}
		args := make([]string, 0, len(rawArgs))
		for _, a := range rawArgs {
			args = append(args, str(a))
		}
		return args, true
	}
	return nil, false
}

// EnforcesAPIKeys reports whether a backend's args enforce API-key validation with the
// expected Katan fixture key: the joined args must carry --validate-keys and name
// --api-keys ... openai=<expectedKey> (in that order). Mirrors assert_enforcement_args.
func EnforcesAPIKeys(args []string, expectedKey string) bool {
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--validate-keys") {
		return false
	}
	i := strings.Index(joined, "--api-keys")
	if i < 0 {
		return false
	}
	return strings.Contains(joined[i:], "openai="+expectedKey)
}

// ServiceSelectorApp returns the spec.selector.app of the named Service.
func ServiceSelectorApp(docs []map[string]any, name string) (string, bool) {
	for _, doc := range docs {
		if str(doc["kind"]) != "Service" || str(dig(doc, "metadata", "name")) != name {
			continue
		}
		return str(dig(doc, "spec", "selector", "app")), true
	}
	return "", false
}

// SecretStringData returns a stringData value for the named Secret.
func SecretStringData(docs []map[string]any, name, key string) (string, bool) {
	for _, doc := range docs {
		if str(doc["kind"]) != "Secret" || str(dig(doc, "metadata", "name")) != name {
			continue
		}
		return str(dig(doc, "stringData", key)), true
	}
	return "", false
}

var katanImageUnpinnedRE = regexp.MustCompile(`llm-katan[:@]`)

// AllKatanImagesDigestPinned reports whether every llm-katan image reference in the raw
// manifest text is pinned by @sha256: digest (no floating tags).
func AllKatanImagesDigestPinned(path string) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if katanImageUnpinnedRE.MatchString(line) && !strings.Contains(line, "@sha256:") {
			return false, nil
		}
	}
	return true, nil
}
