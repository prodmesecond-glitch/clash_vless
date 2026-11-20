//go:build !android && cgo

package gui

import (
	"fmt"
	"image"
	"image/color"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"clashvless/internal/share"
	"clashvless/internal/store"
)

// qrImage renders text as dark modules on white with the standard quiet zone,
// one pixel per module; the canvas scales it up without smoothing.
func qrImage(text string) (image.Image, error) {
	code, err := share.QR(text)
	if err != nil {
		return nil, err
	}
	const quiet = 4
	side := code.Size + 2*quiet
	img := image.NewGray(image.Rect(0, 0, side, side))
	for i := range img.Pix {
		img.Pix[i] = 0xff
	}
	for y := 0; y < code.Size; y++ {
		for x := 0; x < code.Size; x++ {
			if code.Black(x, y) {
				img.SetGray(x+quiet, y+quiet, color.Gray{})
			}
		}
	}
	return img, nil
}

// buildShareCard builds the scrollable view and footer actions for links.
func (d *dashboard) buildShareCard(links []share.Link, skipped int) fyne.CanvasObject {
	if len(links) == 0 {
		return container.NewCenter(muted("Nothing to export"))
	}
	index := 0
	name := widget.NewLabel("")
	name.TextStyle = fyne.TextStyle{Bold: true}
	name.Truncation = fyne.TextTruncateEllipsis
	counter := muted("")
	qrView := canvas.NewImageFromImage(nil)
	qrView.FillMode = canvas.ImageFillContain
	qrView.ScaleMode = canvas.ImageScalePixels
	qrView.SetMinSize(fyne.NewSize(260, 260))
	qrError := wrappingMuted("")
	link := widget.NewLabel("")
	link.Wrapping = fyne.TextWrapBreak
	link.SizeName = theme.SizeNameCaptionText
	note := wrappingMuted("")
	note.Importance = widget.WarningImportance

	var prev, next *widget.Button
	show := func() {
		item := links[index]
		name.SetText(strings.Join(strings.Fields(item.Name), " "))
		counter.Text = fmt.Sprintf("%d of %d", index+1, len(links))
		counter.Refresh()
		if img, err := qrImage(item.URL); err == nil {
			qrView.Image = img
			qrView.Show()
			qrError.Hide()
		} else {
			qrView.Hide()
			qrError.SetText("This link is too long for a QR code; copy it instead.")
			qrError.Show()
		}
		qrView.Refresh()
		link.SetText(item.URL)
		note.SetText(item.Note)
		note.Hidden = item.Note == ""
		note.Refresh()
		if prev != nil {
			if index == 0 {
				prev.Disable()
			} else {
				prev.Enable()
			}
			if index == len(links)-1 {
				next.Disable()
			} else {
				next.Enable()
			}
		}
	}
	copyText := func(value, message string) {
		d.app.Clipboard().SetContent(value)
		d.notify(message)
	}
	actions := container.NewHBox(widget.NewButtonWithIcon("Copy link", theme.ContentCopyIcon(), func() {
		copyText(links[index].URL, "Link copied")
	}))
	header := []fyne.CanvasObject{container.NewBorder(nil, nil, nil, counter, name)}
	if len(links) > 1 {
		prev = widget.NewButtonWithIcon("", theme.NavigateBackIcon(), func() { index--; show() })
		next = widget.NewButtonWithIcon("", theme.NavigateNextIcon(), func() { index++; show() })
		actions = container.NewHBox(prev, next, actions.Objects[0])
	}
	body := append(header,
		container.NewCenter(container.NewStack(canvas.NewRectangle(color.White), qrView)),
		qrError,
		link,
		note,
	)
	footer := []fyne.CanvasObject{actions}
	if len(links) > 1 {
		summary := fmt.Sprintf("%d links", len(links))
		if skipped > 0 {
			summary += fmt.Sprintf(" · %d non-VLESS nodes skipped", skipped)
		}
		footer = append(footer, widget.NewSeparator(), muted(summary), container.NewHBox(
			widget.NewButtonWithIcon("Copy all links", theme.ContentCopyIcon(), func() {
				copyText(share.Joined(links), fmt.Sprintf("%d links copied", len(links)))
			}),
			widget.NewButtonWithIcon("Copy as subscription", theme.ContentCopyIcon(), func() {
				copyText(share.Subscription(links), "Base64 subscription copied")
			}),
		))
	}
	show()
	// The actions stay pinned below; only a long link scrolls.
	scroll := container.NewVScroll(container.NewVBox(body...))
	bottom := container.NewVBox(footer...)
	return container.NewBorder(nil, bottom, nil, nil, scroll)
}

// showShare is a Marzban-style export: a QR code per link with copy actions, and
// whole-list copies (plain or as a base64 subscription) when there are several.
func (d *dashboard) showShare(title string, links []share.Link, skipped int) {
	if len(links) == 0 {
		d.notify("Nothing to export")
		return
	}
	card := d.buildShareCard(links, skipped)
	form := dialog.NewCustom(title, "Close", card, d.window)
	form.Resize(fitScreen(fyne.NewSize(560, 480)))
	d.showDialog(form)
}

// showSubscriptionShare defaults to exporting the subscription URL link via QR code,
// while preserving nodes export as an optional switch in the dialog.
func (d *dashboard) showSubscriptionShare(sub store.Subscription) {
	subLink := share.Link{Name: sub.Name, URL: sub.URL}
	nodeLinks, skipped := share.NodeLinks(sub.Nodes)
	if sub.URL == "" && len(nodeLinks) == 0 {
		d.notify("Nothing to export")
		return
	}
	if sub.URL == "" {
		d.showShare("Export · "+sub.Name, nodeLinks, skipped)
		return
	}
	if len(nodeLinks) == 0 {
		d.showShare("Export · "+sub.Name, []share.Link{subLink}, 0)
		return
	}

	subCard := d.buildShareCard([]share.Link{subLink}, 0)
	nodesCard := d.buildShareCard(nodeLinks, skipped)

	cardContainer := container.NewStack(subCard)

	var subBtn, nodesBtn *widget.Button
	subBtn = widget.NewButton("Subscription link", func() {
		subBtn.Importance = widget.HighImportance
		subBtn.Refresh()
		nodesBtn.Importance = widget.MediumImportance
		nodesBtn.Refresh()
		cardContainer.Objects = []fyne.CanvasObject{subCard}
		cardContainer.Refresh()
	})
	subBtn.Importance = widget.HighImportance

	nodesBtn = widget.NewButton(fmt.Sprintf("Nodes as VLESS (%d)", len(nodeLinks)), func() {
		subBtn.Importance = widget.MediumImportance
		subBtn.Refresh()
		nodesBtn.Importance = widget.HighImportance
		nodesBtn.Refresh()
		cardContainer.Objects = []fyne.CanvasObject{nodesCard}
		cardContainer.Refresh()
	})

	modeBar := container.NewHBox(subBtn, nodesBtn)
	content := container.NewBorder(container.NewVBox(modeBar, widget.NewSeparator()), nil, nil, nil, cardContainer)

	form := dialog.NewCustom("Export · "+sub.Name, "Close", content, d.window)
	form.Resize(fitScreen(fyne.NewSize(560, 520)))
	d.showDialog(form)
}

func linkIcon() fyne.Resource { return brandIcon("link") }
func qrIcon() fyne.Resource   { return brandIcon("qr") }
