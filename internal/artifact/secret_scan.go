package artifact

import (
	"io"
	"regexp"
)

// MaxSecretScanBytes bounds the amount of content inspected synchronously at
// upload time. A bounded scan is an admission signal, not a malware scanner;
// callers must still treat an unquarantined artifact as untrusted input.
const MaxSecretScanBytes int64 = 2 << 20

const secretScanTailBytes = 1024

type secretRule struct {
	name    string
	pattern *regexp.Regexp
}

var secretRules = []secretRule{
	{name: "private_key", pattern: regexp.MustCompile(`-----BEGIN (?:RSA |EC |OPENSSH |DSA )?PRIVATE KEY-----`)},
	{name: "openai_key", pattern: regexp.MustCompile(`\b(?:sk|rk)-[A-Za-z0-9][A-Za-z0-9_-]{19,}\b`)},
	{name: "aws_access_key", pattern: regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{name: "github_token", pattern: regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`)},
	{name: "slack_token", pattern: regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`)},
	{name: "named_secret", pattern: regexp.MustCompile(`(?i)\b(?:api[_-]?key|access[_-]?token|secret|password)\s*[:=]\s*["']?[A-Za-z0-9_\-/.+=]{12,}`)},
}

// secretScanReader observes a bounded prefix while preserving streaming
// behavior. It keeps only a small tail so patterns split across Read calls are
// still detected without retaining the uploaded object in memory.
type secretScanReader struct {
	source  io.Reader
	scanned int64
	tail    []byte
	rule    string
}

func newSecretScanReader(source io.Reader) *secretScanReader {
	return &secretScanReader{source: source}
}

func (reader *secretScanReader) Read(buffer []byte) (int, error) {
	count, err := reader.source.Read(buffer)
	if count <= 0 || reader.rule != "" || reader.scanned >= MaxSecretScanBytes {
		return count, err
	}
	remaining := MaxSecretScanBytes - reader.scanned
	inspectCount := int64(count)
	if inspectCount > remaining {
		inspectCount = remaining
	}
	window := make([]byte, 0, len(reader.tail)+int(inspectCount))
	window = append(window, reader.tail...)
	window = append(window, buffer[:inspectCount]...)
	for _, rule := range secretRules {
		if rule.pattern.Match(window) {
			reader.rule = rule.name
			break
		}
	}
	reader.scanned += inspectCount
	if len(window) > secretScanTailBytes {
		window = window[len(window)-secretScanTailBytes:]
	}
	reader.tail = append(reader.tail[:0], window...)
	return count, err
}

func (reader *secretScanReader) matchedRule() string {
	return reader.rule
}

// scanSecretBytes is intentionally kept package-private; the Service uses
// secretScanReader so large objects remain streamed.
func scanSecretBytes(data []byte) string {
	if int64(len(data)) > MaxSecretScanBytes {
		data = data[:MaxSecretScanBytes]
	}
	for _, rule := range secretRules {
		if rule.pattern.Match(data) {
			return rule.name
		}
	}
	return ""
}
