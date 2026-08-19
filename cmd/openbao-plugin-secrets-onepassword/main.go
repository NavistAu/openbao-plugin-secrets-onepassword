// Command openbao-plugin-secrets-onepassword serves the op secrets
// engine as an OpenBao external plugin process. It is not meant to be
// executed directly; OpenBao's plugin catalog launches it.
package main

import (
	"os"

	"github.com/hashicorp/go-hclog"
	"github.com/openbao/openbao/api/v2"
	"github.com/openbao/openbao/sdk/v2/plugin"

	"github.com/NavistAu/openbao-plugin-secrets-onepassword/backend"
)

func main() {
	apiClientMeta := &api.PluginAPIClientMeta{}
	flags := apiClientMeta.FlagSet()
	if err := flags.Parse(os.Args[1:]); err != nil {
		hclog.L().Error("failed to parse flags", "error", err)
		os.Exit(1)
	}

	tlsProviderFunc := api.VaultPluginTLSProvider(apiClientMeta.GetTLSConfig())

	if err := plugin.ServeMultiplex(&plugin.ServeOpts{
		BackendFactoryFunc: backend.Factory,
		TLSProviderFunc:    tlsProviderFunc,
	}); err != nil {
		hclog.L().Error("plugin shutting down", "error", err)
		os.Exit(1)
	}
}
