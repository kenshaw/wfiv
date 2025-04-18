// Command wfiv is a command-line Google webfonts viewer.
package main

import (
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/gobwas/glob"
	"github.com/kenshaw/colors"
	"github.com/kenshaw/diskcache"
	"github.com/kenshaw/fontimg"
	"github.com/kenshaw/httplog"
	"github.com/kenshaw/rasterm"
	"github.com/kenshaw/webfonts"
	"github.com/tdewolff/canvas"
	"github.com/xo/ox"
	_ "github.com/xo/ox/color"
	gfonts "google.golang.org/api/webfonts/v1"
)

var (
	name    = "wfiv"
	version = "0.0.0-dev"
)

func main() {
	ox.DefaultVersionString = version
	args := &Args{
		logger: func(string, ...any) {},
	}
	ox.RunContext(
		context.Background(),
		ox.Usage(name, "a command-line Google webfonts viewer"),
		ox.Defaults(),
		ox.Sub(
			ox.Usage("list", "show available font families"),
			ox.Exec(listFonts(os.Stdout, args)),
			ox.From(args),
		),
		ox.Sub(
			ox.Usage("show", "show font"),
			ox.Exec(showFonts(os.Stdout, args)),
			ox.From(args),
		),
	)
}

type Args struct {
	Verbose     bool               `ox:"enable verbose,short:v"`
	Key         string             `ox:"webfont api key"`
	All         bool               `ox:"show all"`
	FontSize    uint               `ox:"font preview size,default:48"`
	FontStyle   canvas.FontStyle   `ox:"font preview style"`
	FontVariant canvas.FontVariant `ox:"font preview variant"`
	FontFg      *colors.Color      `ox:"font preview foreground color,default:black"`
	FontBg      *colors.Color      `ox:"font preview background color,default:white"`
	FontDPI     uint               `ox:"font preview dpi,default:100,name:font-dpi"`
	FontMargin  uint               `ox:"font preview margin,default:5"`
	logger      func(string, ...any)
	cache       *diskcache.Cache
}

// listFonts lists the available font families.
func listFonts(w io.Writer, args *Args) func(context.Context, []string) error {
	return func(ctx context.Context, cliargs []string) error {
		if err := args.init(); err != nil {
			return err
		}
		families, err := args.families(ctx)
		if err != nil {
			return err
		}
		for _, family := range families {
			fmt.Fprintf(w, "%s\n", family.Family)
		}
		return nil
	}
}

// showFonts renders the specified files to w.
func showFonts(w io.Writer, args *Args) func(context.Context, []string) error {
	return func(ctx context.Context, cliargs []string) error {
		if !rasterm.Available() {
			return rasterm.ErrTermGraphicsNotAvailable
		}
		if err := args.init(); err != nil {
			return err
		}
		// compile globs
		globs := make([]glob.Glob, len(cliargs))
		for i, arg := range cliargs {
			var err error
			if globs[i], err = glob.Compile(arg); err != nil {
				return fmt.Errorf("bad arg %q (%d): %w", arg, i, err)
			}
		}
		// retrieve faces
		families, err := args.families(ctx)
		if err != nil {
			return err
		}
		// match
		var fonts []*gfonts.Webfont
		if args.All {
			fonts = families
		} else {
			for _, g := range globs {
				for _, f := range families {
					if g.Match(f.Family) {
						fonts = append(fonts, f)
					}
				}
			}
		}
		cl := &http.Client{
			Transport: args.cache,
		}
		for _, f := range fonts {
			fmt.Fprintf(w, "%s:\n", f.Family)
			img, err := args.grab(ctx, cl, f.Family)
			if err != nil {
				fmt.Fprintf(w, "error: %v\n", err)
				continue
			}
			if err := rasterm.Encode(w, img); err != nil {
				fmt.Fprintf(w, "error: %v\n", err)
				continue
			}
		}
		return nil
	}
}

func (args *Args) init() error {
	// args.ctx = ctx
	if args.Key == "" {
		return errors.New("must provide -key")
	}
	// set verbose logger
	if args.Verbose {
		args.logger = func(s string, v ...any) {
			fmt.Fprintf(os.Stderr, s+"\n", v...)
		}
	}
	// create cache transport
	if err := args.buildCache(); err != nil {
		return err
	}
	return nil
}

// buildCache creates a disk cache transport.
func (args *Args) buildCache() error {
	opts := []diskcache.Option{
		diskcache.WithAppCacheDir("webfonts"),
		diskcache.WithTTL(14 * 24 * time.Hour),
		diskcache.WithHeaderWhitelist("Date", "Set-Cookie", "Content-Type", "Location"),
		diskcache.WithErrorTruncator(),
		diskcache.WithGzipCompression(),
	}
	if args.Verbose {
		opts = append(opts, diskcache.WithTransport(
			httplog.NewPrefixedRoundTripLogger(
				http.DefaultTransport,
				args.logger,
				httplog.WithReqResBody(false, false),
			),
		))
	}
	var err error
	args.cache, err = diskcache.New(opts...)
	return err
}

// families retrieves the available webfont families.
func (args *Args) families(ctx context.Context) ([]*gfonts.Webfont, error) {
	return webfonts.Available(ctx, webfonts.WithTransport(args.cache), webfonts.WithKey(args.Key))
}

func (args *Args) grab(ctx context.Context, cl *http.Client, family string) (image.Image, error) {
	face, err := webfonts.WOFF2(ctx, family, webfonts.WithTransport(args.cache))
	switch {
	case err != nil:
		return nil, fmt.Errorf("unable to retrieve woff2 font face: %w", err)
	case face.Src == "":
		return nil, fmt.Errorf("missing face src")
	}
	args.logger("retrieving: %s", face.Src)
	req, err := http.NewRequestWithContext(ctx, "GET", face.Src, nil)
	if err != nil {
		return nil, fmt.Errorf("unable to create http request: %w", err)
	}
	res, err := cl.Do(req)
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve woff2: %w", err)
	}
	defer res.Body.Close()
	if typ := res.Header.Get("Content-Type"); res.StatusCode != http.StatusOK || typ != "font/woff2" {
		return nil, fmt.Errorf("bad woff2 data, status: %d, content-type: %q", res.StatusCode, typ)
	}
	buf, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("unable to read font: %w", err)
	}
	return args.rasterize(buf)
}

func (args *Args) rasterize(buf []byte) (img image.Image, err error) {
	defer func() {
		if e := recover(); e != nil {
			err = fmt.Errorf("caught panic: %v", e)
		}
	}()
	img, err = fontimg.New(buf, "").Rasterize(
		nil,
		int(args.FontSize),
		args.FontStyle,
		args.FontVariant,
		args.FontFg,
		args.FontBg,
		float64(args.FontDPI),
		float64(args.FontMargin),
	)
	return
}
