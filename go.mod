module github.com/jeiang/nb-nix

go 1.26.9

require (
	github.com/netbirdio/netbird v0.81.4
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/apapsch/go-jsonmerge/v2 v2.0.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/oapi-codegen/runtime v1.1.2 // indirect
	github.com/sirupsen/logrus v1.9.4 // indirect
	golang.org/x/sys v0.47.0 // indirect
)

// Copied from github.com/netbirdio/netbird go.mod at the pinned version; replaces do not propagate to dependents.
replace github.com/kardianos/service => github.com/netbirdio/service v0.0.0-20240911161631-f62744f42502

replace github.com/getlantern/systray => github.com/netbirdio/systray v0.0.0-20231030152038-ef1ed2a27949

replace golang.zx2c4.com/wireguard => github.com/netbirdio/wireguard-go v0.0.0-20261008122457-d2b1ce9bfee9

replace github.com/cloudflare/circl => codeberg.org/cunicu/circl v0.0.0-20230801113412-fec58fc7b5f6

replace github.com/pion/ice/v4 => github.com/netbirdio/ice/v4 v4.0.0-20250908184934-6202be846b51

replace github.com/dexidp/dex => github.com/netbirdio/dex v0.244.1-0.20260716205454-a163de3129e5

replace github.com/dexidp/dex/api/v2 => github.com/netbirdio/dex/api/v2 v2.0.0-20260512110716-8d70ad8647c1

replace github.com/mailru/easyjson => github.com/netbirdio/easyjson v0.9.0

replace github.com/wailsapp/wails/v3 => github.com/netbirdio/wails/v3 v3.0.0-beta.3.0.20261009120932-4e087a8bfd6d
