package main

import (
	"embed"
	"log"
	"os"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

// The Vite build is embedded by Wails. Keeping the generated assets in the
// repository also makes the Go package buildable before the first frontend
// build (the placeholder is replaced by `npm run build`).
//
//go:embed all:frontend/dist
var frontendAssets embed.FS

func main() {
	app := NewApp()
	startHidden := hasBackgroundFlag()

	err := wails.Run(&options.App{
		Title:             "GBF Local Cache",
		Width:             1180,
		Height:            760,
		MinWidth:          980,
		MinHeight:         620,
		StartHidden:       startHidden,
		HideWindowOnClose: false,
		AssetServer: &assetserver.Options{
			Assets: frontendAssets,
		},
		OnStartup:     app.startup,
		OnBeforeClose: app.beforeClose,
		OnShutdown:    app.shutdown,
		Bind: []interface{}{
			app,
		},
	})
	if err != nil {
		log.Fatal(err)
	}
}

func hasBackgroundFlag() bool {
	for _, argument := range os.Args[1:] {
		if argument == "--background" {
			return true
		}
	}
	return false
}
