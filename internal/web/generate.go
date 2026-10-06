package web

//go:generate go tool templ generate
//go:generate go run ./icongen -out static/icons
//go:generate tailwindcss --input static/css/input.css --output static/css/app.css --minify
