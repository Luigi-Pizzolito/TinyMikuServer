module github.com/Luigi-Pizzolito/TinyMikuServer/pkg/holocropper

go 1.24.1

replace github.com/Luigi-Pizzolito/TinyMikuServer/pkg/mikvformat => ../mikvformat

require (
	github.com/AlexEidt/Vidio v1.5.1
	github.com/bamiaux/rez v0.0.0-20170731184118-29f4463c688b
	github.com/schollz/progressbar/v3 v3.18.0
)

require (
	github.com/mitchellh/colorstring v0.0.0-20190213212951-d06e56a500db // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	golang.org/x/sys v0.29.0 // indirect
	golang.org/x/term v0.28.0 // indirect
)
