package gui

import (
	"crypto/md5"
	"encoding/hex"
	"os"
	"path/filepath"
)

// iconBytes собирает мульти BMP-ICO 16/24/32/48 32bpp: роутер-«A»
// (два белых маршрута в узел, красная перемычка, три egress-узла)
// на тёмном скруглённом фоне панели. AND нулевой (прозрачность через
// альфу). Совпадает с build/windows/icon.ico (та же геометрия).
func iconBytes() []byte {
	sizes := []int{16, 24, 32, 48}
	type img struct {
		size int
		pix  []byte
		and  []byte
	}
	imgs := make([]img, 0, len(sizes))
	total := 6 + len(sizes)*16
	for _, s := range sizes {
		pix := flipVertical(drawRouterA(s), s)
		// AND маска DWORD aligned: ((s+31)/32)*4 * s, нулевая
		// (32bpp с альфой, прозрачность из альфа-канала).
		rowAnd := ((s + 31) / 32) * 4
		and := make([]byte, rowAnd*s)
		imgs = append(imgs, img{s, pix, and})
		total += 40 + len(pix) + len(and)
	}
	buf := make([]byte, 0, total)
	u16 := func(v uint16) { buf = append(buf, byte(v), byte(v>>8)) }
	u32 := func(v uint32) { buf = append(buf, byte(v), byte(v>>8), byte(v>>16), byte(v>>24)) }
	// ICONDIR
	u16(0)
	u16(1)
	u16(uint16(len(imgs)))
	offset := 6 + len(imgs)*16
	for _, im := range imgs {
		buf = append(buf, byte(im.size), byte(im.size), 0, 0)
		u16(1)
		u16(32)
		u32(uint32(40 + len(im.pix) + len(im.and)))
		u32(uint32(offset))
		offset += 40 + len(im.pix) + len(im.and)
	}
	for _, im := range imgs {
		u32(40)
		u32(uint32(im.size))
		u32(uint32(im.size * 2))
		u16(1)
		u16(32)
		u32(0)
		u32(uint32(len(im.pix) + len(im.and)))
		u32(0)
		u32(0)
		u32(0)
		u32(0)
		buf = append(buf, im.pix...)
		buf = append(buf, im.and...)
	}
	return buf
}

// drawRouterA рисует иконку s×s в BGRA, строки сверху вниз.
// flipVertical переворачивает в BMP bottom-up перед упаковкой.
func flipVertical(pix []byte, s int) []byte {
	row := s * 4
	out := make([]byte, 0, len(pix))
	for y := s - 1; y >= 0; y-- {
		out = append(out, pix[y*row:(y+1)*row]...)
	}
	return out
}
func drawRouterA(s int) []byte {
	f := float64(s)
	pix := make([]byte, s*s*4)
	white := [4]byte{245, 245, 245, 255}
	red := [4]byte{77, 90, 224, 255} // #e5484d в BGRA
	edge := [4]byte{87, 70, 58, 255} // #3a4657 в BGRA
	// Геометрия в долях (та же, что в build/appicon.svg).
	ax, ay := 0.5*f, 0.24*f
	lx, ly := 0.28*f, 0.79*f
	rx, ry := 0.72*f, 0.79*f
	lw := f / 10
	if lw < 2 {
		lw = 2
	}
	bw := f / 15
	if bw < 1.5 {
		bw = 1.5
	}
	nodeR := f * 0.055
	if nodeR < 2 {
		nodeR = 2
	}
	ring := f * 0.02
	if ring < 1 {
		ring = 1
	}
	// Скруглённый фон: inset 6%, радиус 22%.
	m := f * 0.06
	rr := f * 0.22
	segDist := func(px, py, x0, y0, x1, y1 float64) float64 {
		dx := x1 - x0
		dy := y1 - y0
		l2 := dx*dx + dy*dy
		t := 0.0
		if l2 > 0 {
			t = ((px-x0)*dx + (py-y0)*dy) / l2
			if t < 0 {
				t = 0
			} else if t > 1 {
				t = 1
			}
		}
		qx := x0 + t*dx
		qy := y0 + t*dy
		ox := px - qx
		oy := py - qy
		return sqrtF(ox*ox + oy*oy)
	}
	// Знаковое расстояние до скруглённого прямоугольника (m,m)-(f-m,f-m).
	rboxDist := func(px, py float64) float64 {
		qx := px - f/2
		qy := py - f/2
		hx := (f-2*m)/2 - rr
		hy := (f-2*m)/2 - rr
		ox := absF(qx) - hx
		oy := absF(qy) - hy
		axo := ox
		if axo < 0 {
			axo = 0
		}
		ayo := oy
		if ayo < 0 {
			ayo = 0
		}
		out := sqrtF(axo*axo + ayo*ayo)
		inn := ox
		if oy > ox {
			inn = oy
		}
		if inn > 0 {
			inn = 0
		}
		return out + inn - rr
	}
	for y := 0; y < s; y++ {
		for x := 0; x < s; x++ {
			px := float64(x) + 0.5
			py := float64(y) + 0.5
			bd := rboxDist(px, py)
			var c [4]byte
			switch {
			case bd > 0.5:
				c = [4]byte{0, 0, 0, 0} // вне фона — прозрачный
			default:
				// Фон: вертикальный градиент #1a212e -> #0b0e14.
				t := py / f
				c = [4]byte{
					byte(20 + t*(20-20)),
					byte(33 + t*(14-33)),
					byte(46 + t*(20-46)),
					255,
				}
				if bd > -1.5 {
					c = edge // обводка
				} else {
					// Узлы (красные с белым кольцом) — поверх всего.
					drawn := false
					for _, n := range [3][2]float64{{ax, ay}, {lx, ly}, {rx, ry}} {
						ox := px - n[0]
						oy := py - n[1]
						d := sqrtF(ox*ox + oy*oy)
						if d <= nodeR {
							c = red
							drawn = true
							break
						} else if d <= nodeR+ring {
							c = white
							drawn = true
							break
						}
					}
					if !drawn {
						// Перемычка поверх маршрутов.
						if segDist(px, py, 0.39*f, 0.60*f, 0.61*f, 0.60*f) <= bw/2 {
							c = red
						} else if segDist(px, py, lx, ly, ax, ay) <= lw/2 ||
							segDist(px, py, rx, ry, ax, ay) <= lw/2 {
							c = white
						}
					}
				}
			}
			o := (y*s + x) * 4
			pix[o], pix[o+1], pix[o+2], pix[o+3] = c[0], c[1], c[2], c[3]
		}
	}
	return pix
}

func sqrtF(v float64) float64 {
	// Ньютон: иконки мелкие, 8 итераций за глаза; без math-импорта
	// (пакет держим lean — только stdlib уже в файле).
	if v <= 0 {
		return 0
	}
	z := v
	if z < 1 {
		z = 1
	}
	for i := 0; i < 8; i++ {
		z = (z + v/z) / 2
	}
	return z
}

func absF(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
func iconBytesToFilePath(iconBytes []byte) (string, error) {
	bh := md5.Sum(iconBytes)
	dataHash := hex.EncodeToString(bh[:])
	iconFilePath := filepath.Join(os.TempDir(), "systray_temp_icon_"+dataHash+".ico")
	if _, err := os.Stat(iconFilePath); os.IsNotExist(err) {
		if err := os.WriteFile(iconFilePath, iconBytes, 0644); err != nil {
			return "", err
		}
	}
	return iconFilePath, nil
}
