// Package config defines the Bakery command tree and its configuration.
package config

import (
	"net"
	"strconv"
)

// CLI is the root command tree. Kong parses argv and the environment into it.
//
// Later milestones hang their commands here (migrate, sstate push, gc); each is
// a struct with a `cmd:""` tag and a case in main's dispatch.
type CLI struct {
	Serve   ServeCmd   `cmd:"" help:"Run the Bakery cache server."`
	Version VersionCmd `cmd:"" help:"Print the Bakery version and exit."`
}

// ServeCmd configures the HTTP listener.
//
// The env tags are explicit rather than derived from a Kong env prefix, so the
// variable names here are exactly the names in stack.env.tmpl.
type ServeCmd struct {
	Host string `env:"HOST" help:"Interface to bind. Empty binds every interface."`
	Port int    `default:"8080" env:"PORT"  help:"Port to listen on."`
}

// VersionCmd takes no configuration.
type VersionCmd struct{}

// Addr renders the listen address for net.Listen.
func (c ServeCmd) Addr() string {
	return net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
}
