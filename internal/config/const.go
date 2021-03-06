package config

import "time"

const (
	// ModifiedBoot is the message to print when the current boot has been modified
	ModifiedBoot = "zsys-meta:modified-boot"
	// NoModifiedBoot is the message to print when the current boot has no dataset modifications
	NoModifiedBoot = "zsys-meta:no-modified-boot"

	// DefaultSocket path.
	DefaultSocket = "/run/zsysd.sock"

	// DefaultClientTimeout for client requests
	DefaultClientTimeout = 120 * time.Second

	// DefaultServerIdleTimeout is the default time without a request before the server exits
	DefaultServerIdleTimeout = time.Minute

	// DefaultPath is the default configuration path
	DefaultPath = "/etc/zsys.conf"

	// UserConfirmationNeeded is a dedicated type for GRPC error which signal that we need more info from user
	UserConfirmationNeeded = "UserConfirmationNeeded"
)

var (
	// Version is the version of the executable
	Version = "dev"
)
