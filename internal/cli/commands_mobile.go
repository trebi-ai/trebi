// Paired phones (plan 2026-10-09 B8): pair a phone with a QR code, then
// list, rename, change access, or remove it.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/mdp/qrterminal/v3"
	"github.com/trebi-ai/trebi/internal/request"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/urfave/cli/v3"
)

var errPaired = errors.New("paired")

func mobileCommand() *cli.Command {
	return &cli.Command{
		Name:  "mobile",
		Usage: "paired phones and tablets (the Trebi app)",
		Commands: []*cli.Command{
			{
				Name:  "pair",
				Usage: "show a pairing link and QR code for the Trebi app",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "name", Usage: "name for the phone"},
					&cli.StringFlag{Name: "ttl", Usage: "how long the link works, 1m to 720h (default 10m)"},
					&cli.IntFlag{Name: "uses", Usage: "how many phones can use the link, 1 to 10 (default 1)"},
					&cli.BoolFlag{Name: "no-wait", Usage: "return at once; do not wait for the phone"},
					&cli.BoolFlag{Name: "json", Usage: "emit JSON (implies --no-wait)"},
				},
				Action: runMobilePair,
			},
			{
				Name:    "ls",
				Aliases: []string{"list"},
				Usage:   "list the paired phones",
				Flags:   []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
				Action:  runMobileList,
			},
			{
				Name:      "rename",
				Usage:     "rename a paired phone",
				ArgsUsage: "<id> <name>",
				Flags:     []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
				Action: func(_ context.Context, c *cli.Command) error {
					name := c.Args().Get(1)
					return runMobileUpdate(c, "usage: trebi mobile rename <id> <name>", &name, nil)
				},
			},
			{
				Name:      "scope",
				Usage:     "give a phone full control or view only access",
				ArgsUsage: "<id> full|read",
				Flags:     []cli.Flag{&cli.BoolFlag{Name: "json", Usage: "emit JSON"}},
				Action: func(_ context.Context, c *cli.Command) error {
					scope := c.Args().Get(1)
					return runMobileUpdate(c, "usage: trebi mobile scope <id> full|read", nil, &scope)
				},
			},
			{
				Name:      "rm",
				Aliases:   []string{"remove"},
				Usage:     "remove a paired phone",
				ArgsUsage: "<id>",
				Flags:     []cli.Flag{forceFlag("required; the phone loses access and must pair again")},
				Action:    runMobileRemove,
			},
			{
				Name:      "cancel",
				Usage:     "close an open pairing link",
				ArgsUsage: "<pairing-id>",
				Action:    runMobileCancel,
			},
		},
	}
}

func runMobilePair(ctx context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var offer struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Link      string `json:"link"`
		ExpiresAt string `json:"expires_at"`
		Uses      int    `json:"uses"`
	}
	req := request.BuildMobilePair(request.MobilePair{Name: c.String("name"), TTL: c.String("ttl"), Uses: c.Int("uses")})
	if err := client.DoHTTP(req, &offer); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, offer)
	}
	w := outWriter(c)
	fmt.Fprintln(w, "Scan this code with the camera of the phone, or open the link on the phone.")
	qrterminal.GenerateWithConfig(offer.Link, qrterminal.Config{Level: qrterminal.L, Writer: w, HalfBlocks: true,
		BlackChar: qrterminal.BLACK_BLACK, WhiteBlackChar: qrterminal.WHITE_BLACK, WhiteChar: qrterminal.WHITE_WHITE, BlackWhiteChar: qrterminal.BLACK_WHITE, QuietZone: 1})
	fmt.Fprintf(w, "Link: %s\n", offer.Link)
	fmt.Fprintf(w, "The link works until %s. Do not share it.\n", offer.ExpiresAt)
	if c.Bool("no-wait") {
		return nil
	}
	fmt.Fprintln(w, "Waiting for the phone. Press Ctrl-C to stop.")
	err = client.StreamEvents(ctx, request.BuildStreamPath("mobile.paired", "latest"), func(ev store.StreamEvent) error {
		var a struct {
			Name    string `json:"name"`
			OfferID string `json:"offer_id"`
		}
		if json.Unmarshal(ev.Attrs, &a) != nil || a.OfferID != offer.ID {
			return nil
		}
		fmt.Fprintf(w, "Paired: %s\n", a.Name)
		return errPaired
	})
	if errors.Is(err, errPaired) {
		return nil
	}
	return err
}

func runMobileList(_ context.Context, c *cli.Command) error {
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body struct {
		Items []map[string]any `json:"items"`
	}
	if err := client.GetJSON(request.BuildMobileClientsPath(), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	tbl := newTable(outWriter(c), []string{"ID", "Name", "Platform", "Access", "Connected", "Last seen"})
	for _, m := range body.Items {
		connected := "no"
		if v, _ := m["connected"].(bool); v {
			connected = "yes"
		}
		tbl.AppendRow(table.Row{mapStr(m, "id"), mapStr(m, "name"), mapStr(m, "platform"), mapStr(m, "scope"), connected, dash(mapStr(m, "last_seen_at"))})
	}
	renderTable(tbl)
	return nil
}

func runMobileUpdate(c *cli.Command, usage string, name, scope *string) error {
	id := c.Args().Get(0)
	if id == "" || c.Args().Get(1) == "" {
		return errors.New(usage)
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := client.DoHTTP(request.BuildMobileUpdate(id, name, scope), &body); err != nil {
		return err
	}
	if c.Bool("json") {
		return encodeJSON(c, body)
	}
	fmt.Fprintf(outWriter(c), "Phone %s: %s, access %s.\n", id, mapStr(body, "name"), mapStr(body, "scope"))
	return nil
}

func runMobileRemove(_ context.Context, c *cli.Command) error {
	id := c.Args().Get(0)
	if id == "" {
		return errors.New("usage: trebi mobile rm <id>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	confirm, err := confirmed(c, "removes phone "+id+"; it loses access to this machine and must pair again")
	if err != nil {
		return err
	}
	if err := client.DoHTTP(request.BuildMobileRemove(id, confirm), nil); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "Phone %s removed.\n", id)
	return nil
}

func runMobileCancel(_ context.Context, c *cli.Command) error {
	id := c.Args().Get(0)
	if id == "" {
		return errors.New("usage: trebi mobile cancel <pairing-id>")
	}
	client, err := clientFor(c)
	if err != nil {
		return err
	}
	if err := client.DoHTTP(request.BuildMobilePairCancel(id), nil); err != nil {
		return err
	}
	fmt.Fprintf(outWriter(c), "Pairing %s closed.\n", id)
	return nil
}
