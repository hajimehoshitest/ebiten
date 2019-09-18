// Copyright 2014 Hajime Hoshi
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ebiten

import (
	"fmt"
	"image"
	"image/color"
	"math"
	"sync"

	"github.com/hajimehoshi/ebiten/internal/driver"
	"github.com/hajimehoshi/ebiten/internal/graphics"
)

var (
	// imageQueue represents a queue for image operations that are ordered before the game starts (BeginFrame).
	// Before the game starts, the package shareable doesn't determine the minimum/maximum texture sizes (#879).
	// Instead of accessing the package shareable, defer the image operations until the game starts (#921).
	imageQueue           []func()
	imageQueueM          sync.Mutex
	needsEnqueueImageOps = true
)

func checkNeedsEnqueueImageOp(location string) {
	imageQueueM.Lock()
	defer imageQueueM.Unlock()

	if needsEnqueueImageOps {
		panic(fmt.Sprintf("ebiten: %s is not available before the game starts", location))
	}
}

func enqueueImageOpIfNeeded(f func() func()) bool {
	imageQueueM.Lock()
	defer imageQueueM.Unlock()

	if !needsEnqueueImageOps {
		return false
	}
	imageQueue = append(imageQueue, f())
	return true
}

func flushImageOpsIfNeeded() {
	imageQueueM.Lock()

	if !needsEnqueueImageOps {
		if len(imageQueue) > 0 {
			panic("ebiten: len(imageQueue) must be 0 after the game starts")
		}
		imageQueueM.Unlock()
		return
	}

	// Set this flag false first, or the image operations will be queued again.
	needsEnqueueImageOps = false
	imageQueueM.Unlock()

	// As a new item will not be enqueued any longer, mutex does not have to, or should not be used.
	for _, f := range imageQueue {
		f()
	}
	imageQueue = nil
}

// Image represents a rectangle set of pixels.
// The pixel format is alpha-premultiplied RGBA.
// Image implements image.Image and draw.Image.
//
// Functions of Image never returns error as of 1.5.0-alpha, and error values are always nil.
type Image struct {
	// addr holds self to check copying.
	// See strings.Builder for similar examples.
	addr *Image

	// mipmap is a set of shareable.Image sorted by the order of mipmap level.
	// The level 0 image is a regular image and higher-level images are used for mipmap.
	mipmap *mipmap

	bounds   image.Rectangle
	original *Image

	pendingPixels []byte

	filter Filter
}

func (i *Image) copyCheck() {
	if i.addr != i {
		panic("ebiten: illegal use of non-zero Image copied by value")
	}
}

// Size returns the size of the image.
func (i *Image) Size() (width, height int) {
	s := i.Bounds().Size()
	return s.X, s.Y
}

func (i *Image) isDisposed() bool {
	return i.mipmap.isDisposed()
}

func (i *Image) isSubImage() bool {
	return i.original != nil
}

// Clear resets the pixels of the image into 0.
//
// When the image is disposed, Clear does nothing.
//
// Clear always returns nil as of 1.5.0-alpha.
func (i *Image) Clear() error {
	i.Fill(color.Transparent)
	return nil
}

// Fill fills the image with a solid color.
//
// When the image is disposed, Fill does nothing.
//
// Fill always returns nil as of 1.5.0-alpha.
func (i *Image) Fill(clr color.Color) error {
	i.copyCheck()

	if enqueueImageOpIfNeeded(func() func() {
		r, g, b, a := clr.RGBA()
		return func() {
			i.Fill(color.RGBA64{
				R: uint16(r),
				G: uint16(g),
				B: uint16(b),
				A: uint16(a),
			})
		}
	}) {
		return nil
	}

	if i.isDisposed() {
		return nil
	}

	// TODO: Implement this.
	if i.isSubImage() {
		panic("ebiten: render to a subimage is not implemented (Fill)")
	}

	i.resolvePendingPixels(false)

	i.mipmap.fill(clr)
	return nil
}

func (i *Image) disposeMipmaps() {
	if i.isDisposed() {
		panic("ebiten: the image is already disposed at disposeMipmap")
	}
	i.mipmap.disposeMipmaps()
}

// DrawImage draws the given image on the image i.
//
// DrawImage accepts the options. For details, see the document of
// DrawImageOptions.
//
// For drawing, the pixels of the argument image at the time of this call is
// adopted. Even if the argument image is mutated after this call, the drawing
// result is never affected.
//
// When the image i is disposed, DrawImage does nothing.
// When the given image img is disposed, DrawImage panics.
//
// When the given image is as same as i, DrawImage panics.
//
// DrawImage works more efficiently as batches
// when the successive calls of DrawImages satisfy the below conditions:
//
//   * All render targets are same (A in A.DrawImage(B, op))
//   * Either all ColorM element values are same or all the ColorM have only
//      diagonal ('scale') elements
//     * If only (*ColorM).Scale is applied to a ColorM, the ColorM has only
//       diagonal elements. The other ColorM functions might modify the other
//       elements.
//   * All CompositeMode values are same
//   * All Filter values are same
//
// Even when all the above conditions are satisfied, multiple draw commands can
// be used in really rare cases. Ebiten images usually share an internal
// automatic texture atlas, but when you consume the atlas, or you create a huge
// image, those images cannot be on the same texture atlas. In this case, draw
// commands are separated. The texture atlas size is 4096x4096 so far. Another
// case is when you use an offscreen as a render source. An offscreen doesn't
// share the texture atlas with high probability.
//
// For more performance tips, see https://ebiten.org/performancetips.html
//
// DrawImage always returns nil as of 1.5.0-alpha.
func (i *Image) DrawImage(img *Image, options *DrawImageOptions) error {
	i.copyCheck()

	if enqueueImageOpIfNeeded(func() func() {
		op := *options
		return func() {
			i.DrawImage(img, &op)
		}
	}) {
		return nil
	}

	if img.isDisposed() {
		panic("ebiten: the given image to DrawImage must not be disposed")
	}
	if i.isDisposed() {
		return nil
	}

	// TODO: Implement this.
	if i.isSubImage() {
		panic("ebiten: render to a subimage is not implemented (drawImage)")
	}

	img.resolvePendingPixels(true)
	i.resolvePendingPixels(true)

	// Calculate vertices before locking because the user can do anything in
	// options.ImageParts interface without deadlock (e.g. Call Image functions).
	if options == nil {
		options = &DrawImageOptions{}
	}

	parts := options.ImageParts
	// Parts is deprecated. This implementations is for backward compatibility.
	if parts == nil && options.Parts != nil {
		parts = imageParts(options.Parts)
	}

	// ImageParts is deprecated. This implementations is for backward compatibility.
	if parts != nil {
		l := parts.Len()
		for idx := 0; idx < l; idx++ {
			sx0, sy0, sx1, sy1 := parts.Src(idx)
			dx0, dy0, dx1, dy1 := parts.Dst(idx)
			op := &DrawImageOptions{
				ColorM:        options.ColorM,
				CompositeMode: options.CompositeMode,
				Filter:        options.Filter,
			}
			op.GeoM.Scale(
				float64(dx1-dx0)/float64(sx1-sx0),
				float64(dy1-dy0)/float64(sy1-sy0))
			op.GeoM.Translate(float64(dx0), float64(dy0))
			op.GeoM.Concat(options.GeoM)
			i.DrawImage(img.SubImage(image.Rect(sx0, sy0, sx1, sy1)).(*Image), op)
		}
		return nil
	}

	bounds := img.Bounds()

	// SourceRect is deprecated. This implementation is for backward compatibility.
	if options.SourceRect != nil {
		bounds = bounds.Intersect(*options.SourceRect)
		if bounds.Empty() {
			return nil
		}
	}

	geom := &options.GeoM
	mode := driver.CompositeMode(options.CompositeMode)

	filter := driver.FilterNearest
	if options.Filter != FilterDefault {
		filter = driver.Filter(options.Filter)
	} else if img.filter != FilterDefault {
		filter = driver.Filter(img.filter)
	}

	if det := geom.det(); det == 0 {
		return nil
	} else if math.IsNaN(float64(det)) {
		return nil
	}

	level := img.mipmap.mipmapLevel(geom, bounds.Dx(), bounds.Dy(), filter)

	if level > 0 {
		// If the image can be scaled into 0 size, adjust the level. (#839)
		w, h := bounds.Dx(), bounds.Dy()
		for level >= 0 {
			s := 1 << uint(level)
			if w/s == 0 || h/s == 0 {
				level--
				continue
			}
			break
		}

		if level < 0 {
			// As the render source is too small, nothing is rendered.
			return nil
		}
	}

	if level > 6 {
		level = 6
	}
	if level < -6 {
		level = -6
	}

	// TODO: Add (*mipmap).drawImage and move the below code.
	colorm := options.ColorM.impl
	cr, cg, cb, ca := float32(1), float32(1), float32(1), float32(1)
	if colorm.ScaleOnly() {
		body, _ := colorm.UnsafeElements()
		cr = body[0]
		cg = body[5]
		cb = body[10]
		ca = body[15]
		colorm = nil
	}

	a, b, c, d, tx, ty := geom.elements()
	if level == 0 {
		src := img.mipmap.original()
		vs := vertexSlice(4)
		graphics.PutQuadVertices(vs, src, bounds.Min.X, bounds.Min.Y, bounds.Max.X, bounds.Max.Y, a, b, c, d, tx, ty, cr, cg, cb, ca)
		is := graphics.QuadIndices()
		i.mipmap.original().DrawTriangles(src, vs, is, colorm, mode, filter, driver.AddressClampToZero)
	} else if src := img.mipmap.level(bounds, level); src != nil {
		w, h := src.Size()
		s := pow2(level)
		a *= s
		b *= s
		c *= s
		d *= s
		vs := vertexSlice(4)
		graphics.PutQuadVertices(vs, src, 0, 0, w, h, a, b, c, d, tx, ty, cr, cg, cb, ca)
		is := graphics.QuadIndices()
		i.mipmap.original().DrawTriangles(src, vs, is, colorm, mode, filter, driver.AddressClampToZero)
	}
	i.disposeMipmaps()
	return nil
}

// Vertex represents a vertex passed to DrawTriangles.
//
// Note that this API is experimental.
type Vertex struct {
	// DstX and DstY represents a point on a destination image.
	DstX float32
	DstY float32

	// SrcX and SrcY represents a point on a source image.
	// Be careful that SrcX/SrcY coordinates are on the image's bounds.
	// This means that a left-upper point of a sub-image might not be (0, 0).
	SrcX float32
	SrcY float32

	// ColorR/ColorG/ColorB/ColorA represents color scaling values.
	// 1 means the original source image color is used.
	// 0 means a transparent color is used.
	ColorR float32
	ColorG float32
	ColorB float32
	ColorA float32
}

// Address represents a sampler address mode.
type Address int

const (
	// AddressClampToZero means that out-of-range texture coordinates return 0 (transparent).
	AddressClampToZero Address = Address(driver.AddressClampToZero)

	// AddressRepeat means that texture coordinates wrap to the other side of the texture.
	AddressRepeat Address = Address(driver.AddressRepeat)
)

// DrawTrianglesOptions represents options to render triangles on an image.
//
// Note that this API is experimental.
type DrawTrianglesOptions struct {
	// ColorM is a color matrix to draw.
	// The default (zero) value is identity, which doesn't change any color.
	// ColorM is applied before vertex color scale is applied.
	ColorM ColorM

	// CompositeMode is a composite mode to draw.
	// The default (zero) value is regular alpha blending.
	CompositeMode CompositeMode

	// Filter is a type of texture filter.
	// The default (zero) value is FilterDefault.
	Filter Filter

	// Address is a sampler address mode.
	// The default (zero) value is AddressClampToZero.
	Address Address
}

// MaxIndicesNum is the maximum number of indices for DrawTriangles.
const MaxIndicesNum = graphics.IndicesNum

// DrawTriangles draws a triangle with the specified vertices and their indices.
//
// If len(indices) is not multiple of 3, DrawTriangles panics.
//
// If len(indices) is more than MaxIndicesNum, DrawTriangles panics.
//
// The rule in which DrawTriangles works effectively is same as DrawImage's.
//
// When the image i is disposed, DrawTriangles does nothing.
//
// Internal mipmap is not used on DrawTriangles.
//
// Note that this API is experimental.
func (i *Image) DrawTriangles(vertices []Vertex, indices []uint16, img *Image, options *DrawTrianglesOptions) {
	i.copyCheck()

	if enqueueImageOpIfNeeded(func() func() {
		vs := make([]Vertex, len(vertices))
		copy(vs, vertices)
		is := make([]uint16, len(indices))
		copy(is, indices)
		op := *options
		return func() {
			i.DrawTriangles(vs, is, img, &op)
		}
	}) {
		return
	}

	if i.isDisposed() {
		return
	}

	if i.isSubImage() {
		panic("ebiten: render to a subimage is not implemented (DrawTriangles)")
	}

	img.resolvePendingPixels(true)
	i.resolvePendingPixels(true)

	if len(indices)%3 != 0 {
		panic("ebiten: len(indices) % 3 must be 0")
	}
	if len(indices) > MaxIndicesNum {
		panic("ebiten: len(indices) must be <= MaxIndicesNum")
	}
	// TODO: Check the maximum value of indices and len(vertices)?

	if options == nil {
		options = &DrawTrianglesOptions{}
	}

	mode := driver.CompositeMode(options.CompositeMode)

	filter := driver.FilterNearest
	if options.Filter != FilterDefault {
		filter = driver.Filter(options.Filter)
	} else if img.filter != FilterDefault {
		filter = driver.Filter(img.filter)
	}

	vs := vertexSlice(len(vertices))
	src := img.mipmap.original()
	r := img.Bounds()
	for idx, v := range vertices {
		src.PutVertex(vs[idx*graphics.VertexFloatNum:(idx+1)*graphics.VertexFloatNum],
			float32(v.DstX), float32(v.DstY), v.SrcX, v.SrcY,
			float32(r.Min.X), float32(r.Min.Y), float32(r.Max.X), float32(r.Max.Y),
			v.ColorR, v.ColorG, v.ColorB, v.ColorA)
	}
	i.mipmap.original().DrawTriangles(src, vs, indices, options.ColorM.impl, mode, filter, driver.Address(options.Address))
	i.disposeMipmaps()
}

// SubImage returns an image representing the portion of the image p visible through r. The returned value shares pixels with the original image.
//
// The returned value is always *ebiten.Image.
//
// If the image is disposed, SubImage returns nil.
//
// In the current Ebiten implementation, SubImage is available only as a rendering source.
func (i *Image) SubImage(r image.Rectangle) image.Image {
	i.copyCheck()
	if i.isDisposed() {
		return nil
	}

	img := &Image{
		mipmap: i.mipmap,
		filter: i.filter,
	}

	// Keep the original image's reference not to dispose that by GC.
	if i.isSubImage() {
		img.original = i.original
	} else {
		img.original = i
	}
	img.addr = img

	r = r.Intersect(i.Bounds())
	// Need to check Empty explicitly. See the standard image package implementations.
	if r.Empty() {
		img.bounds = image.ZR
	} else {
		img.bounds = r
	}
	return img
}

// Bounds returns the bounds of the image.
func (i *Image) Bounds() image.Rectangle {
	if i.isDisposed() {
		panic("ebiten: the image is already disposed")
	}
	if !i.isSubImage() {
		w, h := i.mipmap.size()
		return image.Rect(0, 0, w, h)
	}
	return i.bounds
}

// ColorModel returns the color model of the image.
func (i *Image) ColorModel() color.Model {
	return color.RGBAModel
}

// At returns the color of the image at (x, y).
//
// At loads pixels from GPU to system memory if necessary, which means that At can be slow.
//
// At always returns a transparent color if the image is disposed.
//
// Note that important logic should not rely on values returned by At, since
// the returned values can include very slight differences between some machines.
//
// At can't be called outside the main loop (ebiten.Run's updating function) starts (as of version 1.4.0-alpha).
func (i *Image) At(x, y int) color.Color {
	checkNeedsEnqueueImageOp("(*Image).At")

	if i.isDisposed() {
		return color.RGBA{}
	}
	if i.isSubImage() && !image.Pt(x, y).In(i.bounds) {
		return color.RGBA{}
	}
	i.resolvePendingPixels(true)
	r, g, b, a := i.mipmap.at(x, y)
	return color.RGBA{r, g, b, a}
}

// Set sets the color at (x, y).
//
// Set loads pixels from GPU to system memory if necessary, which means that Set can be slow.
//
// Set can't be called outside the main loop (ebiten.Run's updating function) starts.
//
// If the image is disposed, Set does nothing.
func (img *Image) Set(x, y int, clr color.Color) {
	checkNeedsEnqueueImageOp("(*Image).Set")

	img.copyCheck()
	if img.isDisposed() {
		return
	}
	if img.isSubImage() && !image.Pt(x, y).In(img.bounds) {
		return
	}
	if img.isSubImage() {
		img = img.original
	}

	w, h := img.Size()
	if img.pendingPixels == nil {
		pix := make([]byte, 4*w*h)
		idx := 0
		for j := 0; j < h; j++ {
			for i := 0; i < w; i++ {
				r, g, b, a := img.mipmap.at(i, j)
				pix[4*idx] = r
				pix[4*idx+1] = g
				pix[4*idx+2] = b
				pix[4*idx+3] = a
				idx++
			}
		}
		img.pendingPixels = pix
	}
	r, g, b, a := clr.RGBA()
	img.pendingPixels[4*(x+y*w)] = byte(r >> 8)
	img.pendingPixels[4*(x+y*w)+1] = byte(g >> 8)
	img.pendingPixels[4*(x+y*w)+2] = byte(b >> 8)
	img.pendingPixels[4*(x+y*w)+3] = byte(a >> 8)
}

func (i *Image) resolvePendingPixels(draw bool) {
	if i.isSubImage() {
		i.original.resolvePendingPixels(draw)
		return
	}

	if i.pendingPixels == nil {
		return
	}

	if !draw {
		i.pendingPixels = nil
		return
	}

	i.ReplacePixels(i.pendingPixels)
	i.pendingPixels = nil
}

// Dispose disposes the image data. After disposing, most of image functions do nothing and returns meaningless values.
//
// Calling Dispose is not mandatory. GC automatically collects internal resources that no objects refer to.
// However, calling Dispose explicitly is helpful if memory usage matters.
//
// When the image is disposed, Dipose does nothing.
//
// Dipose always return nil as of 1.5.0-alpha.
func (i *Image) Dispose() error {
	i.copyCheck()

	if enqueueImageOpIfNeeded(func() func() {
		return func() {
			i.Dispose()
		}
	}) {
		return nil
	}

	if i.isDisposed() {
		return nil
	}
	if i.isSubImage() {
		return nil
	}
	i.mipmap.dispose()
	i.resolvePendingPixels(false)
	return nil
}

// ReplacePixels replaces the pixels of the image with p.
//
// The given p must represent RGBA pre-multiplied alpha values. len(p) must equal to 4 * (image width) * (image height).
//
// ReplacePixels may be slow (as for implementation, this calls glTexSubImage2D).
//
// When len(p) is not appropriate, ReplacePixels panics.
//
// When the image is disposed, ReplacePixels does nothing.
//
// ReplacePixels always returns nil as of 1.5.0-alpha.
func (i *Image) ReplacePixels(p []byte) error {
	i.copyCheck()

	if enqueueImageOpIfNeeded(func() func() {
		px := make([]byte, len(p))
		copy(px, p)
		return func() {
			i.ReplacePixels(px)
		}
	}) {
		return nil
	}

	if i.isDisposed() {
		return nil
	}
	// TODO: Implement this.
	if i.isSubImage() {
		panic("ebiten: render to a subimage is not implemented (ReplacePixels)")
	}
	i.resolvePendingPixels(false)
	s := i.Bounds().Size()
	if l := 4 * s.X * s.Y; len(p) != l {
		panic(fmt.Sprintf("ebiten: len(p) was %d but must be %d", len(p), l))
	}
	i.mipmap.replacePixels(p)
	return nil
}

// A DrawImageOptions represents options to render an image on an image.
type DrawImageOptions struct {
	// GeoM is a geometry matrix to draw.
	// The default (zero) value is identify, which draws the image at (0, 0).
	GeoM GeoM

	// ColorM is a color matrix to draw.
	// The default (zero) value is identity, which doesn't change any color.
	ColorM ColorM

	// CompositeMode is a composite mode to draw.
	// The default (zero) value is regular alpha blending.
	CompositeMode CompositeMode

	// Filter is a type of texture filter.
	// The default (zero) value is FilterDefault.
	//
	// Filter can also be specified at NewImage* functions, but
	// specifying filter at DrawImageOptions is recommended (as of 1.7.0-alpha).
	//
	// If both Filter specified at NewImage* and DrawImageOptions are FilterDefault,
	// FilterNearest is used.
	// If either is FilterDefault and the other is not, the latter is used.
	// Otherwise, Filter specified at DrawImageOptions is used.
	Filter Filter

	// Deprecated (as of 1.5.0-alpha): Use SubImage instead.
	ImageParts ImageParts

	// Deprecated (as of 1.1.0-alpha): Use SubImage instead.
	Parts []ImagePart

	// Deprecated (as of 1.9.0-alpha): Use SubImage instead.
	SourceRect *image.Rectangle
}

// NewImage returns an empty image.
//
// If width or height is less than 1 or more than device-dependent maximum size, NewImage panics.
//
// filter argument is just for backward compatibility.
// If you are not sure, specify FilterDefault.
//
// Error returned by NewImage is always nil as of 1.5.0-alpha.
func NewImage(width, height int, filter Filter) (*Image, error) {
	i := &Image{
		mipmap: newMipmap(width, height),
		filter: filter,
	}
	i.addr = i
	return i, nil
}

// makeVolatile makes the image 'volatile'.
// A volatile image is always cleared at the start of a frame.
//
// This is suitable for offscreen images that pixels are changed often.
//
// Regular non-volatile images need to record drawing history or read its pixels from GPU if necessary so that all
// the images can be restored automatically from the context lost. However, such recording the drawing history or
// reading pixels from GPU are expensive operations. Volatile images can skip such oprations, but the image content
// is cleared every frame instead.
//
// When the image is disposed, makeVolatile does nothing.
func (i *Image) makeVolatile() {
	if enqueueImageOpIfNeeded(func() func() {
		return func() {
			i.makeVolatile()
		}
	}) {
		return
	}

	if i.isDisposed() {
		return
	}
	i.mipmap.makeVolatile()
}

// NewImageFromImage creates a new image with the given image (source).
//
// If source's width or height is less than 1 or more than device-dependent maximum size, NewImageFromImage panics.
//
// filter argument is just for backward compatibility.
// If you are not sure, specify FilterDefault.
//
// Error returned by NewImageFromImage is always nil as of 1.5.0-alpha.
func NewImageFromImage(source image.Image, filter Filter) (*Image, error) {
	size := source.Bounds().Size()

	width, height := size.X, size.Y

	i := &Image{
		mipmap: newMipmap(width, height),
		filter: filter,
	}
	i.addr = i

	_ = i.ReplacePixels(copyImage(source))
	return i, nil
}

func newScreenFramebufferImage(width, height int) *Image {
	i := &Image{
		mipmap: newScreenFramebufferMipmap(width, height),
		filter: FilterDefault,
	}
	i.addr = i
	return i
}

// MaxImageSize is deprecated as of 1.7.0-alpha. No replacement so far.
//
// TODO: Make this replacement (#541)
var MaxImageSize = 4096
