package config

import (
	"testing"

	"github.com/alecthomas/kong"
)

func TestServeCmdBinding(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		args []string
		want ServeCmd
		addr string
	}{
		{
			name: "defaults",
			args: []string{"serve"},
			want: ServeCmd{Host: "", Port: 8080},
			addr: ":8080",
		},
		{
			name: "env vars land in the struct",
			env:  map[string]string{"HOST": "127.0.0.1", "PORT": "9999"},
			args: []string{"serve"},
			want: ServeCmd{Host: "127.0.0.1", Port: 9999},
			addr: "127.0.0.1:9999",
		},
		{
			name: "PORT alone leaves HOST empty",
			env:  map[string]string{"PORT": "1234"},
			args: []string{"serve"},
			want: ServeCmd{Host: "", Port: 1234},
			addr: ":1234",
		},
		{
			name: "flags beat env",
			env:  map[string]string{"HOST": "127.0.0.1", "PORT": "9999"},
			args: []string{"serve", "--host", "0.0.0.0", "--port", "3000"},
			want: ServeCmd{Host: "0.0.0.0", Port: 3000},
			addr: "0.0.0.0:3000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			var cli CLI

			parser, err := kong.New(&cli, kong.Name("bakery"))
			if err != nil {
				t.Fatalf("build parser: %v", err)
			}

			kctx, err := parser.Parse(tt.args)
			if err != nil {
				t.Fatalf("parse %v: %v", tt.args, err)
			}

			if got := kctx.Command(); got != "serve" {
				t.Fatalf("got command %q, want %q", got, "serve")
			}

			if cli.Serve != tt.want {
				t.Errorf("got %+v, want %+v", cli.Serve, tt.want)
			}

			if got := cli.Serve.Addr(); got != tt.addr {
				t.Errorf("Addr() = %q, want %q", got, tt.addr)
			}
		})
	}
}

func TestCommandTree(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "serve", args: []string{"serve"}, want: "serve"},
		{name: "version", args: []string{"version"}, want: "version"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cli CLI

			parser, err := kong.New(&cli, kong.Name("bakery"))
			if err != nil {
				t.Fatalf("build parser: %v", err)
			}

			kctx, err := parser.Parse(tt.args)
			if err != nil {
				t.Fatalf("parse %v: %v", tt.args, err)
			}

			if got := kctx.Command(); got != tt.want {
				t.Errorf("got command %q, want %q", got, tt.want)
			}
		})
	}
}
