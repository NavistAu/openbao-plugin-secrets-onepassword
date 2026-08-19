// Bench-only OpenBao server config (Task 8 bench gate). Not used in
// production — the estate installs this plugin via svc.openbao
// tofu/ansible (Task 10), never this file.
storage "file" {
  path = "/bao/data"
}

listener "tcp" {
  address     = "0.0.0.0:8200"
  tls_disable = true
}

plugin_directory = "/bao/plugins"

api_addr     = "http://127.0.0.1:8200"
cluster_addr = "http://127.0.0.1:8201"

ui = false
log_level = "info"
