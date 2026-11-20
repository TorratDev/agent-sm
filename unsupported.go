//go:build !unix

package main

// The runner and the agent step use process groups, sh and symlinks. See README.md, "Platforms".
var _ = agentSM_builds_only_on_Linux_and_macOS__use_WSL_on_Windows
