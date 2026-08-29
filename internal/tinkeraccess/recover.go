package tinkeraccess

import (
	"fmt"
	"os"
	"regexp"
)

// usersPathRe matches the Leptos server-fn route for get_users, e.g.
// "api/get_users11102523982452806591". The digit suffix is the collision hash
// the #[server] macro appends; it changes if the fn moves module or server_fn
// is upgraded.
var usersPathRe = regexp.MustCompile(`api/get_users[0-9]{6,}`)

// RecoverUsersPath scans a compiled Leptos WASM bundle for the current
// get_users route and returns it as a leading-slash path. Point wasmPath at
// something like
// tinker-access-server-rs/target/site/pkg/tinker-access-server-rs.wasm.
func RecoverUsersPath(wasmPath string) (string, error) {
	data, err := os.ReadFile(wasmPath)
	if err != nil {
		return "", fmt.Errorf("tinkeraccess: read wasm: %w", err)
	}
	m := usersPathRe.Find(data)
	if m == nil {
		return "", fmt.Errorf("tinkeraccess: no get_users route found in %s", wasmPath)
	}
	return "/" + string(m), nil
}
