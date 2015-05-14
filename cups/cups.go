/*
Copyright 2015 Google Inc. All rights reserved.

Use of this source code is governed by a BSD-style
license that can be found in the LICENSE file or at
https://developers.google.com/open-source/licenses/bsd
*/
package cups

/*
#cgo LDFLAGS: -lcups
#include <cups/cups.h>
#include <stdlib.h> // free
#include "cups.h"
*/
import "C"
import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/google/cups-connector/cdd"
	"github.com/google/cups-connector/lib"

	"github.com/golang/glog"
)

const (
	// CUPS "URL" length are always less than 40. For example: /job/1234567
	urlMaxLength = 100

	attrPrinterName         = "printer-name"
	attrPrinterInfo         = "printer-info"
	attrPrinterMakeAndModel = "printer-make-and-model"
	attrPrinterUUID         = "printer-uuid"
	attrPrinterState        = "printer-state"
	attrPrinterStateReasons = "printer-state-reasons"
	attrMarkerNames         = "marker-names"
	attrMarkerTypes         = "marker-types"
	attrMarkerLevels        = "marker-levels"

	attrJobState                = "job-state"
	attrJobMediaSheetsCompleted = "job-media-sheets-completed"
)

var (
	requiredPrinterAttributes []string = []string{
		attrPrinterName,
		attrPrinterInfo,
		attrPrinterMakeAndModel,
		attrPrinterUUID,
		attrPrinterState,
		attrPrinterStateReasons,
		attrMarkerNames,
		attrMarkerTypes,
		attrMarkerLevels,
	}

	jobAttributes []string = []string{
		attrJobState,
		attrJobMediaSheetsCompleted,
	}
)

// Interface between Go and the CUPS API.
type CUPS struct {
	cc                *cupsCore
	pc                *ppdCache
	infoToDisplayName bool
	printerAttributes []string
	systemTags        map[string]string
}

func NewCUPS(infoToDisplayName bool, printerAttributes []string, maxConnections uint, connectTimeout time.Duration) (*CUPS, error) {
	if err := checkPrinterAttributes(printerAttributes); err != nil {
		return nil, err
	}

	cc, err := newCUPSCore(maxConnections, connectTimeout)
	if err != nil {
		return nil, err
	}
	pc := newPPDCache(cc)

	systemTags, err := getSystemTags()
	if err != nil {
		return nil, err
	}

	c := &CUPS{cc, pc, infoToDisplayName, printerAttributes, systemTags}

	return c, nil
}

func (c *CUPS) Quit() {
	c.pc.quit()
}

// ConnQtyOpen gets the current quantity of open CUPS connections.
func (c *CUPS) ConnQtyOpen() uint {
	return c.cc.connQtyOpen()
}

// ConnQtyOpen gets the maximum quantity of open CUPS connections.
func (c *CUPS) ConnQtyMax() uint {
	return c.cc.connQtyMax()
}

// GetPrinters gets all CUPS printers found on the CUPS server.
func (c *CUPS) GetPrinters() ([]lib.Printer, error) {
	pa := C.newArrayOfStrings(C.int(len(c.printerAttributes)))
	defer C.freeStringArrayAndStrings(pa, C.int(len(c.printerAttributes)))
	for i, a := range c.printerAttributes {
		C.setStringArrayValue(pa, C.int(i), C.CString(a))
	}

	response, err := c.cc.getPrinters(pa, C.int(len(c.printerAttributes)))
	if err != nil {
		return nil, err
	}

	// cupsDoRequest() returns ipp_t pointer which needs explicit free.
	defer C.ippDelete(response)

	if C.ippGetStatusCode(response) == C.IPP_STATUS_ERROR_NOT_FOUND {
		// Normal error when there are no printers.
		return make([]lib.Printer, 0), nil
	}

	printers := c.responseToPrinters(response)
	for i := range printers {
		printers[i].GCPVersion = lib.GCPAPIVersion
		printers[i].ConnectorVersion = lib.ShortName
	}
	c.addPPDHashToPrinters(printers)

	return printers, nil
}

// responseToPrinters converts a C.ipp_t to a slice of lib.Printers.
func (c *CUPS) responseToPrinters(response *C.ipp_t) []lib.Printer {
	printers := make([]lib.Printer, 0, 1)

	for a := C.ippFirstAttribute(response); a != nil; a = C.ippNextAttribute(response) {
		if C.ippGetGroupTag(a) != C.IPP_TAG_PRINTER {
			continue
		}

		attributes := make([]*C.ipp_attribute_t, 0, C.int(len(c.printerAttributes)))
		for ; a != nil && C.ippGetGroupTag(a) == C.IPP_TAG_PRINTER; a = C.ippNextAttribute(response) {
			attributes = append(attributes, a)
		}
		tags := attributesToTags(attributes)
		p := tagsToPrinter(tags, c.systemTags, c.infoToDisplayName)

		printers = append(printers, p)
	}

	return printers
}

// addPPDHashToPrinters fetches PPD hashes for all printers concurrently.
func (c *CUPS) addPPDHashToPrinters(printers []lib.Printer) {
	var wg sync.WaitGroup

	for i := range printers {
		if !lib.PrinterIsRaw(printers[i]) {
			wg.Add(1)
			go func(p *lib.Printer) {
				if ppdHash, err := c.pc.getPPDHash(p.Name); err == nil {
					p.CapsHash = ppdHash
				} else {
					glog.Error(err)
				}
				wg.Done()
			}(&printers[i])
		}
	}

	wg.Wait()
}

func getSystemTags() (map[string]string, error) {
	tags := make(map[string]string)

	tags["connector-version"] = lib.BuildDate
	hostname, err := os.Hostname()
	if err == nil {
		tags["system-hostname"] = hostname
	}
	tags["system-arch"] = runtime.GOARCH

	sysname, nodename, release, version, machine, err := uname()
	if err != nil {
		return nil, fmt.Errorf("CUPS failed to call uname while initializing: %s", err)
	}

	tags["system-uname-sysname"] = sysname
	tags["system-uname-nodename"] = nodename
	tags["system-uname-release"] = release
	tags["system-uname-version"] = version
	tags["system-uname-machine"] = machine

	tags["connector-cups-client-version"] = fmt.Sprintf("%d.%d.%d",
		C.CUPS_VERSION_MAJOR, C.CUPS_VERSION_MINOR, C.CUPS_VERSION_PATCH)

	return tags, nil
}

// GetPPD gets the PPD for the specified printer.
func (c *CUPS) GetPPD(printername string) (string, string, string, error) {
	ppd, err := c.pc.getPPD(printername)
	if err != nil {
		return "", "", "", err
	}

	manufacturer, model := parseManufacturerAndModel(ppd)

	return ppd, manufacturer, model, nil
}

// RemoveCachedPPD removes a printer's PPD from the cache.
func (c *CUPS) RemoveCachedPPD(printername string) {
	c.pc.removePPD(printername)
}

// GetJobState gets the current state of the job indicated by jobID.
func (c *CUPS) GetJobState(jobID uint32) (cdd.PrintJobStateDiff, error) {
	ja := C.newArrayOfStrings(C.int(len(jobAttributes)))
	defer C.freeStringArrayAndStrings(ja, C.int(len(jobAttributes)))
	for i, attribute := range jobAttributes {
		C.setStringArrayValue(ja, C.int(i), C.CString(attribute))
	}

	response, err := c.cc.getJobAttributes(C.int(jobID), ja)
	if err != nil {
		return cdd.PrintJobStateDiff{}, err
	}

	// cupsDoRequest() returned ipp_t pointer needs explicit free.
	defer C.ippDelete(response)

	s := C.ippFindAttribute(response, C.JOB_STATE, C.IPP_TAG_ENUM)
	state := int32(C.ippGetInteger(s, C.int(0)))

	p := C.ippFindAttribute(response, C.JOB_MEDIA_SHEETS_COMPLETED, C.IPP_TAG_INTEGER)
	var pages int32
	if p != nil {
		pages = int32(C.ippGetInteger(p, C.int(0)))
	}

	return convertJobState(state, pages), nil
}

// convertJobState converts CUPS job state to cdd.PrintJobStateDiff.
func convertJobState(cupsState, pages int32) cdd.PrintJobStateDiff {
	state := cdd.PrintJobStateDiff{PagesPrinted: pages}

	switch cupsState {
	case 3: // PENDING
		state.State = cdd.JobState{Type: "IN_PROGRESS"}
	case 4: // HELD
		state.State = cdd.JobState{Type: "IN_PROGRESS"}
	case 5: // PROCESSING
		state.State = cdd.JobState{Type: "IN_PROGRESS"}
	case 6: // STOPPED
		state.State = cdd.JobState{
			Type:              "STOPPED",
			DeviceActionCause: &cdd.DeviceActionCause{ErrorCode: "OTHER"},
		}
	case 7: // CANCELED
		state.State = cdd.JobState{
			Type:            "ABORTED",
			UserActionCause: &cdd.UserActionCause{ActionCode: "CANCELLED"}, // Spelled with two L's.
		}
	case 8: // ABORTED
		state.State = cdd.JobState{
			Type:              "ABORTED",
			DeviceActionCause: &cdd.DeviceActionCause{ErrorCode: "PRINT_FAILURE"},
		}
	case 9: // COMPLETED
		state.State = cdd.JobState{Type: "DONE"}
	}

	return state
}

// Print sends a new print job to the specified printer. The job ID
// is returned.
func (c *CUPS) Print(printername, filename, title, user string, ticket cdd.CloudJobTicket) (uint32, error) {
	pn := C.CString(printername)
	defer C.free(unsafe.Pointer(pn))
	fn := C.CString(filename)
	defer C.free(unsafe.Pointer(fn))
	t := C.CString(title)
	defer C.free(unsafe.Pointer(t))

	options := ticketToOptions(ticket)
	numOptions := C.int(0)
	var o *C.cups_option_t = nil
	for key, value := range options {
		k, v := C.CString(key), C.CString(value)
		numOptions = C.cupsAddOption(k, v, numOptions, &o)
		C.free(unsafe.Pointer(k))
		C.free(unsafe.Pointer(v))
	}
	defer C.cupsFreeOptions(numOptions, o)

	u := C.CString(user)
	defer C.free(unsafe.Pointer(u))

	jobID, err := c.cc.printFile(u, pn, fn, t, numOptions, o)
	if err != nil {
		return 0, err
	}

	return uint32(jobID), nil
}

func ticketToOptions(ticket cdd.CloudJobTicket) map[string]string {
	m := make(map[string]string)

	for _, vti := range ticket.Print.VendorTicketItem {
		m[vti.ID] = vti.Value
	}
	if ticket.Print.Color != nil {
		switch ticket.Print.Color.Type {
		case "CUSTOM_COLOR", "CUSTOM_MONOCHROME":
			m["ColorModel"] = ticket.Print.Color.VendorID
		default:
			m["ColorModel"] = ticket.Print.Color.Type
		}
	}
	if ticket.Print.Duplex != nil {
		switch ticket.Print.Duplex.Type {
		case "LONG_EDGE":
			m["Duplex"] = "DuplexNoTumble"
		case "SHORT_EDGE":
			m["Duplex"] = "DuplexTumble"
		case "NO_DUPLEX":
			m["Duplex"] = "None"
		}
	}
	if ticket.Print.PageOrientation != nil {
		switch ticket.Print.PageOrientation.Type {
		case "PORTRAIT":
			m["orientation-requested"] = "3"
		case "LANDSCAPE":
			m["orientation-requested"] = "4"
		}
	}
	if ticket.Print.Copies != nil {
		m["copies"] = strconv.FormatInt(int64(ticket.Print.Copies.Copies), 10)
	}
	if ticket.Print.Margins != nil {
		m["page-top"] = micronsToPoints(ticket.Print.Margins.TopMicrons)
		m["page-right"] = micronsToPoints(ticket.Print.Margins.RightMicrons)
		m["page-bottom"] = micronsToPoints(ticket.Print.Margins.BottomMicrons)
		m["page-left"] = micronsToPoints(ticket.Print.Margins.LeftMicrons)
	}
	if ticket.Print.DPI != nil {
		if ticket.Print.DPI.VendorID != "" {
			m["Resolution"] = ticket.Print.DPI.VendorID
		} else {
			m["Resolution"] = fmt.Sprintf("%dx%xdpi",
				ticket.Print.DPI.HorizontalDPI, ticket.Print.DPI.VerticalDPI)
		}
	}
	if ticket.Print.FitToPage != nil {
		switch ticket.Print.FitToPage.Type {
		case "FIT_TO_PAGE":
			m["fit-to-page"] = "true"
		case "NO_FITTING":
			m["fit-to-page"] = "false"
		}
	}
	if ticket.Print.PageRange != nil && len(ticket.Print.PageRange.Interval) > 0 {
		pageRanges := make([]string, 0, len(ticket.Print.PageRange.Interval))
		for _, interval := range ticket.Print.PageRange.Interval {
			if interval.End == 0 {
				pageRanges = append(pageRanges, fmt.Sprintf("%d", interval.Start))
			} else {
				pageRanges = append(pageRanges, fmt.Sprintf("%d-%d", interval.Start, interval.End))
			}
		}
		m["page-ranges"] = strings.Join(pageRanges, ",")
	}
	if ticket.Print.MediaSize != nil {
		m["media"] = ticket.Print.MediaSize.VendorID
	}
	if ticket.Print.Collate != nil {
		if ticket.Print.Collate.Collate {
			m["Collate"] = "true"
		} else {
			m["Collate"] = "false"
		}
	}
	if ticket.Print.ReverseOrder != nil {
		if ticket.Print.ReverseOrder.ReverseOrder {
			m["outputorder"] = "reverse"
		} else {
			m["outputorder"] = "normal"
		}
	}

	return m
}

func micronsToPoints(microns int32) string {
	return strconv.Itoa(int(float32(microns)*72/25400 + 0.5))
}

// convertIPPDateToTime converts an RFC 2579 date to a time.Time object.
func convertIPPDateToTime(date *C.ipp_uchar_t) time.Time {
	r := bytes.NewReader(C.GoBytes(unsafe.Pointer(date), 11))
	var year uint16
	var month, day, hour, min, sec, dsec uint8
	binary.Read(r, binary.BigEndian, &year)
	binary.Read(r, binary.BigEndian, &month)
	binary.Read(r, binary.BigEndian, &day)
	binary.Read(r, binary.BigEndian, &hour)
	binary.Read(r, binary.BigEndian, &min)
	binary.Read(r, binary.BigEndian, &sec)
	binary.Read(r, binary.BigEndian, &dsec)

	var utcDirection, utcHour, utcMin uint8
	binary.Read(r, binary.BigEndian, &utcDirection)
	binary.Read(r, binary.BigEndian, &utcHour)
	binary.Read(r, binary.BigEndian, &utcMin)

	var utcOffset time.Duration
	utcOffset += time.Duration(utcHour) * time.Hour
	utcOffset += time.Duration(utcMin) * time.Minute
	var loc *time.Location
	if utcDirection == '-' {
		loc = time.FixedZone("", -int(utcOffset.Seconds()))
	} else {
		loc = time.FixedZone("", int(utcOffset.Seconds()))
	}

	nsec := int(dsec) * 100 * int(time.Millisecond)

	return time.Date(int(year), time.Month(month), int(day), int(hour), int(min), int(sec), nsec, loc)
}

// attributesToTags converts a slice of C.ipp_attribute_t to a
// string:string "tag" map. Outside of this package, "printer attributes" are
// known as "tags".
func attributesToTags(attributes []*C.ipp_attribute_t) map[string][]string {
	tags := make(map[string][]string)

	for _, a := range attributes {
		key := C.GoString(C.ippGetName(a))
		count := int(C.ippGetCount(a))
		values := make([]string, count)

		switch C.ippGetValueTag(a) {
		case C.IPP_TAG_NOVALUE, C.IPP_TAG_NOTSETTABLE:
			// No value means no value.

		case C.IPP_TAG_INTEGER, C.IPP_TAG_ENUM:
			for i := 0; i < count; i++ {
				values[i] = strconv.FormatInt(int64(C.ippGetInteger(a, C.int(i))), 10)
			}

		case C.IPP_TAG_BOOLEAN:
			for i := 0; i < count; i++ {
				if int(C.ippGetInteger(a, C.int(i))) == 0 {
					values[i] = "false"
				} else {
					values[i] = "true"
				}
			}

		case C.IPP_TAG_STRING, C.IPP_TAG_TEXT, C.IPP_TAG_NAME, C.IPP_TAG_KEYWORD, C.IPP_TAG_URI, C.IPP_TAG_CHARSET, C.IPP_TAG_LANGUAGE, C.IPP_TAG_MIMETYPE:
			for i := 0; i < count; i++ {
				values[i] = C.GoString(C.ippGetString(a, C.int(i), nil))
			}

		case C.IPP_TAG_DATE:
			for i := 0; i < count; i++ {
				date := C.ippGetDate(a, C.int(i))
				t := convertIPPDateToTime(date)
				values[i] = strconv.FormatInt(t.Unix(), 10)
			}

		case C.IPP_TAG_RESOLUTION:
			for i := 0; i < count; i++ {
				yres := C.int(-1)
				unit := C.int(-1)
				xres := C.ippGetResolutionWrapper(a, C.int(i), &yres, &unit)
				if unit == C.IPP_RES_PER_CM {
					values[i] = fmt.Sprintf("%dx%dpp%s", int(xres), int(yres), "cm")
				} else {
					values[i] = fmt.Sprintf("%dx%dpp%s", int(xres), int(yres), "i")
				}
			}

		case C.IPP_TAG_RANGE:
			for i := 0; i < count; i++ {
				uppervalue := C.int(-1)
				lowervalue := C.ippGetRange(a, C.int(i), &uppervalue)
				values[i] = fmt.Sprintf("%d~%d", int(lowervalue), int(uppervalue))
			}

		default:
			if count > 0 {
				values = []string{"unknown or unsupported type"}
			}
		}

		if len(values) == 1 && values[0] == "none" {
			values = []string{}
		}
		// This block fixes some drivers' marker types, which list an extra
		// type containing a comma, which CUPS interprets as an extra type.
		// The extra type starts with a space, so it's easy to detect.
		if len(values) > 1 && len(values[len(values)-1]) > 1 && values[len(values)-1][0:1] == " " {
			newValues := make([]string, len(values)-1)
			for i := 0; i < len(values)-2; i++ {
				newValues[i] = values[i]
			}
			newValues[len(newValues)-1] = strings.Join(values[len(values)-2:], ",")
			values = newValues
		}
		tags[key] = values
	}

	return tags
}

// tagsToPrinter converts a map of tags to a Printer.
func tagsToPrinter(printerTags map[string][]string, systemTags map[string]string, infoToDisplayName bool) lib.Printer {
	tags := make(map[string]string)

	for k, v := range printerTags {
		tags[k] = strings.Join(v, ",")
	}
	for k, v := range systemTags {
		tags[k] = v
	}

	var name string
	if n, ok := printerTags[attrPrinterName]; ok {
		name = n[0]
	}
	var uuid string
	if u, ok := printerTags[attrPrinterUUID]; ok {
		uuid = u[0]
	}

	state := cdd.PrinterStateSection{}

	if s, ok := printerTags[attrPrinterState]; ok {
		switch s[0] {
		case "3":
			state.State = "IDLE"
		case "4":
			state.State = "PROCESSING"
		case "5":
			state.State = "STOPPED"
		default:
			state.State = "IDLE"
		}
	}

	if reasons, ok := printerTags[attrPrinterStateReasons]; ok && len(reasons) > 0 {
		sort.Strings(reasons)
		state.VendorState = &cdd.VendorState{Item: make([]cdd.VendorStateItem, len(reasons))}
		for i, reason := range reasons {
			vendorState := cdd.VendorStateItem{DescriptionLocalized: cdd.NewLocalizedString(reason)}
			if strings.HasSuffix(reason, "-error") {
				vendorState.State = "ERROR"
			} else if strings.HasSuffix(reason, "-warning") {
				vendorState.State = "WARNING"
			} else if strings.HasSuffix(reason, "-report") {
				vendorState.State = "INFO"
			} else {
				vendorState.State = "INFO"
			}
			state.VendorState.Item[i] = vendorState
		}
	}

	markers, markerState := convertMarkers(printerTags[attrMarkerNames], printerTags[attrMarkerTypes], printerTags[attrMarkerLevels])
	state.MarkerState = markerState

	p := lib.Printer{
		Name:    name,
		UUID:    uuid,
		State:   state,
		Markers: markers,
		Tags:    tags,
	}
	p.SetTagshash()

	if pi, ok := printerTags[attrPrinterInfo]; ok && infoToDisplayName {
		p.DefaultDisplayName = pi[0]
	}

	return p
}

// convertMarkers converts CUPS marker-(names|types|levels) to *[]cdd.Marker and *cdd.MarkerState.
//
// Normalizes marker type: toner(Cartridge|-cartridge) => toner,
// ink(Cartridge|-cartridge|Ribbon|-ribbon) => ink
func convertMarkers(names, types, levels []string) (*[]cdd.Marker, *cdd.MarkerState) {
	if len(names) == 0 || len(types) == 0 || len(levels) == 0 {
		return nil, nil
	}
	if len(names) != len(types) || len(types) != len(levels) {
		glog.Warningf("Received badly-formatted markers from CUPS: %s, %s, %s",
			strings.Join(names, ";"), strings.Join(types, ";"), strings.Join(levels, ";"))
		return nil, nil
	}

	markers := make([]cdd.Marker, len(names))
	states := cdd.MarkerState{make([]cdd.MarkerStateItem, len(names))}
	for i := 0; i < len(names); i++ {
		if len(names[i]) == 0 {
			return nil, nil
		}
		n := names[i]
		t := types[i]
		switch t {
		case "tonerCartridge", "toner-cartridge":
			t = "toner"
		case "inkCartridge", "ink-cartridge", "ink-ribbon", "inkRibbon":
			t = "ink"
		}
		l, err := strconv.ParseInt(levels[i], 10, 32)
		if err != nil {
			glog.Warningf("Failed to parse CUPS marker state %s=%s: %s", n, levels[i], err)
			return nil, nil
		}

		if l < 0 {
			// The CUPS driver doesn't know what the levels are; not useful.
			return nil, nil
		} else if l > 100 {
			// Lop off extra (proprietary?) bits.
			l = l & 0x7f
			if l > 100 {
				// Even that didn't work.
				return nil, nil
			}
		}

		markers[i] = newMarker(n, t)
		states.Item[i] = newMarkerStateItem(n, int32(l))
	}

	return &markers, &states
}

func newMarker(vendorID, vendorType string) cdd.Marker {
	marker := cdd.Marker{VendorID: vendorID}

	switch vendorType {
	case "toner", "ink", "staples":
		marker.Type = strings.ToUpper(vendorType)
	default:
		marker.Type = "CUSTOM"
		marker.CustomDisplayNameLocalized = cdd.NewLocalizedString(vendorType)
	}

	if marker.Type == "TONER" || marker.Type == "INK" {
		switch strings.ToLower(vendorID) {
		case "black", "color", "cyan", "magenta", "yellow", "light_cyan", "light_magenta",
			"gray", "light_gray", "pigment_black", "matte_black", "photo_cyan", "photo_magenta",
			"photo_yellow", "photo_gray", "red", "green", "blue":
			// Colors known to CDD Marker.Color enum.
			marker.Color = &cdd.MarkerColor{Type: strings.ToUpper(vendorID)}
		default:
			marker.Color = &cdd.MarkerColor{Type: "CUSTOM", CustomDisplayNameLocalized: cdd.NewLocalizedString(vendorID)}
		}
	}

	return marker
}

func newMarkerStateItem(vendorID string, vendorLevel int32) cdd.MarkerStateItem {
	var state string
	if vendorLevel > 10 {
		state = "OK"
	} else {
		state = "EXHAUSTED"
	}

	return cdd.MarkerStateItem{VendorID: vendorID, State: state, LevelPercent: vendorLevel}
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if needle == h {
			return true
		}
	}
	return false
}

func findMissing(haystack, needles []string) []string {
	missing := make([]string, 0)
	for _, n := range needles {
		if !contains(haystack, n) {
			missing = append(missing, n)
		}
	}
	return missing
}

func checkPrinterAttributes(printerAttributes []string) error {
	if !contains(printerAttributes, "all") {
		missing := findMissing(printerAttributes, requiredPrinterAttributes)
		if len(missing) > 0 {
			return fmt.Errorf("Printer attributes missing from config file: %s",
				strings.Join(missing, ","))
		}
	}

	return nil
}
