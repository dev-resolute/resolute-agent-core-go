module github.com/resolute-sh/pi-core-agent-go

go 1.24

require github.com/resolute-sh/pi-llm-go v0.1.0

require (
	github.com/bahlo/generic-list-go v0.2.0 // indirect
	github.com/buger/jsonparser v1.1.2 // indirect
	github.com/invopop/jsonschema v0.14.0 // indirect
	github.com/pb33f/ordered-map/v2 v2.3.1 // indirect
	go.yaml.in/yaml/v4 v4.0.0-rc.2 // indirect
)

replace github.com/resolute-sh/pi-llm-go => ../pi-llm-go
