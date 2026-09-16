package main

import "fmt"

// resolvConf builds the content of /etc/resolv.conf pointing at the guest's
// single nameserver (the host's DNS resolver, see internal/dns).
func resolvConf(dnsIP string) string {
	return fmt.Sprintf("nameserver %s\n", dnsIP)
}

// hostsFile builds the content of /etc/hosts: just enough for the guest's
// own hostname (its machine ID) to resolve locally.
func hostsFile(machineID string) string {
	return fmt.Sprintf("127.0.0.1\tlocalhost\n127.0.0.1\t%s\n", machineID)
}

// defaultPath is the PATH a child gets when neither the image nor the app
// config set one: the same default Docker gives a container.
const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// baselineEnv returns the environment a child of oak-init is seeded with
// before the image's own env and the app config/secrets are merged on top
// (they win on collision). It mirrors what a container runtime guarantees a
// process and that software takes for granted: PATH, so a bare argv[0] can
// be resolved, and HOME, which Erlang's filename:basedir/3, Python's uv,
// git and many others read unconditionally and fail hard without. oak-init
// runs the child as root regardless of the image's USER, so HOME defaults
// to root's, exactly what Docker sets for a root process whose image left
// it unset.
func baselineEnv(env map[string]string) []string {
	path := env["PATH"]
	if path == "" {
		path = defaultPath
	}
	home := env["HOME"]
	if home == "" {
		home = "/root"
	}
	return []string{"PATH=" + path, "HOME=" + home}
}
