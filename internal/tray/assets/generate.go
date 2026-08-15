//go:build ignore

package main

import (
	"image"
	"image/color"
	"image/png"
	"os"
)

const size = 22

func main() {
	write("healthy.png", func(x, y int) bool {
		dx, dy := x-11, y-11
		distance := dx*dx + dy*dy
		return distance >= 36 && distance <= 81
	})
	write("attention.png", func(x, y int) bool {
		if y < 2 || y > 19 {
			return false
		}
		half := (y - 2) / 2
		inside := x >= 11-half && x <= 11+half
		if !inside {
			return false
		}
		return !((x == 10 || x == 11) && (y >= 8 && y <= 14 || y >= 17 && y <= 18))
	})
	write("offline.png", func(x, y int) bool {
		if x < 3 || x > 18 || y < 3 || y > 18 {
			return false
		}
		return abs(x-y) <= 1 || abs((size-1-x)-y) <= 1
	})
}

func write(path string, draw func(x, y int) bool) {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	black := color.NRGBA{A: 255}
	for y := range size {
		for x := range size {
			if draw(x, y) {
				img.SetNRGBA(x, y, black)
			}
		}
	}
	file, err := os.Create(path)
	if err != nil {
		panic(err)
	}
	if err := png.Encode(file, img); err != nil {
		file.Close()
		panic(err)
	}
	if err := file.Close(); err != nil {
		panic(err)
	}
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
