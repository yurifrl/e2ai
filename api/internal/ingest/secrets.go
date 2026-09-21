// Per-app secret rendering (APPS-03/SECR-01): the registry stores op://
// refs only; the 1Password CLI renders them at boot. Go never holds a
// plaintext secret that outlives the process — the resolver output feeds
// the in-memory per-app secret map and nothing else.
package ingest

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// OpSecretResolver renders an op:// ref to plaintext by shelling out to
// `op read --force` (the 1Password CLI — the same renderer `op inject`
// uses for .env). An empty or non-op:// ref resolves to "" and the app
// falls back to the shared WEBHOOK_SECRET.
func OpSecretResolver() SecretResolver {
	return func(ctx context.Context, ref string) (string, error) {
		ref = strings.TrimSpace(ref)
		if ref == "" || !strings.HasPrefix(ref, "op://") {
			return "", nil
		}
		cmd := exec.CommandContext(ctx, "op", "read", "--force", ref)
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = nil // keep the secret out of error text
		if err := cmd.Run(); err != nil {
			// The secret text must never leak into logs: report only
			// that resolution failed for the ref shape.
			return "", fmt.Errorf("op read %s: %v", ref[:min(len(ref), 12)]+"…", err)
		}
		return strings.TrimRight(out.String(), "\r\n"), nil
	}
}
