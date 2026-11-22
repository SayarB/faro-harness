package main

import (
	"embed"
	"log"

	"github.com/wailsapp/wails/v3/pkg/application"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	svc, err := NewApp()
	if err != nil {
		log.Fatal(err)
	}

	// Seams for later: a system tray is `app.SystemTray.New()` here, and the built-in updater is
	// `app.Updater.Init(...)` before app.Run(). For a tray that keeps the indexer alive with no
	// window, also set ApplicationShouldTerminateAfterLastWindowClosed to false below.
	app := application.New(application.Options{
		Name:        "Faro",
		Description: "Local-first AI harness for your files",
		Services: []application.Service{
			application.NewService(svc),
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		Mac: application.MacOptions{
			// Closing the window quits the app (the Wails v2 behaviour).
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID: "com.faro.harness.desktop",
			// A second launch brings the running window to the front.
			OnSecondInstanceLaunch: func(application.SecondInstanceData) {
				if w := svc.window; w != nil {
					w.Show()
					w.UnMinimise()
					w.Focus()
				}
			},
		},
	})
	svc.app = app

	svc.window = app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:            "Faro",
		Width:            1120,
		Height:           760,
		MinWidth:         720,
		MinHeight:        520,
		BackgroundColour: application.NewRGB(15, 17, 23),
		URL:              "/",
	})

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
