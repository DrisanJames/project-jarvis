package pmta

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// AcctParser reads and parses PMTA accounting CSV files.
// PMTA writes delivery/bounce/feedback records in CSV format:
//
//	type,timeLogged,orig,rcpt,orcpt,dsnAction,dsnStatus,dsnDiag,dsnMTA,
//	bounceCat,srcType,srcMTA,dlvType,dlvSourceIp,dlvDestinationIp,dlvEsmtpAvailable,
//	dlvSize,vmta,jobId,envId,queue,vmtaPool,header_X-Campaign-ID,...
type AcctParser struct {
	headerMap map[string]int // column name -> index
}

// NewAcctParser returns a parser. Call ParseFile or ParseReader to process records.
func NewAcctParser() *AcctParser {
	return &AcctParser{}
}

// ParseFile reads a PMTA accounting CSV from disk.
func (p *AcctParser) ParseFile(path string) ([]AcctRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cannot open accounting file %s: %w", path, err)
	}
	defer f.Close()
	return p.ParseReader(f)
}

// ParseReader reads accounting records from any io.Reader.
func (p *AcctParser) ParseReader(r io.Reader) ([]AcctRecord, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

	var records []AcctRecord
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		line := scanner.Text()

		if strings.HasPrefix(line, "#") {
			if strings.HasPrefix(line, "#type,") {
				p.parseHeader(line[1:]) // strip leading #
			}
			continue
		}

		if strings.TrimSpace(line) == "" {
			continue
		}

		rec, err := p.parseLine(line)
		if err != nil {
			continue // skip malformed lines
		}
		records = append(records, rec)
	}

	if err := scanner.Err(); err != nil {
		return records, fmt.Errorf("error reading accounting data: %w", err)
	}

	return records, nil
}

func (p *AcctParser) parseHeader(line string) {
	fields := strings.Split(line, ",")
	p.headerMap = make(map[string]int, len(fields))
	for i, f := range fields {
		p.headerMap[strings.TrimSpace(f)] = i
	}
}

func (p *AcctParser) field(fields []string, name string) string {
	if p.headerMap == nil {
		return ""
	}
	idx, ok := p.headerMap[name]
	if !ok || idx >= len(fields) {
		return ""
	}
	return strings.TrimSpace(fields[idx])
}

func (p *AcctParser) parseLine(line string) (AcctRecord, error) {
	fields := strings.Split(line, ",")
	if len(fields) < 5 {
		return AcctRecord{}, fmt.Errorf("too few fields: %d", len(fields))
	}

	// If we have a header map, use named fields; otherwise fall back to positional.
	if p.headerMap != nil {
		return p.parseNamed(fields)
	}
	return p.parsePositional(fields)
}

func (p *AcctParser) parseNamed(fields []string) (AcctRecord, error) {
	ts, err := time.Parse("2006-01-02 15:04:05", p.field(fields, "timeLogged"))
	if err != nil {
		ts = time.Now()
	}

	rcpt := p.field(fields, "rcpt")
	domain := ""
	if idx := strings.LastIndex(rcpt, "@"); idx >= 0 {
		domain = strings.ToLower(rcpt[idx+1:])
	}

	return AcctRecord{
		Type:       p.field(fields, "type"),
		TimeLogged: ts,
		Orig:       p.field(fields, "orig"),
		Rcpt:       rcpt,
		SourceIP:   p.field(fields, "dlvSourceIp"),
		VMTA:       p.field(fields, "vmta"),
		JobID:      p.field(fields, "jobId"),
		Domain:     domain,
		BounceCode: p.field(fields, "dsnStatus"),
		DSNDiag:    p.field(fields, "dsnDiag"),
		BounceCat:  p.field(fields, "bounceCat"),
		MessageID:  p.field(fields, "header_Message-ID"),
		DKIMResult: p.field(fields, "dkimResult"),
	}, nil
}

func (p *AcctParser) parsePositional(fields []string) (AcctRecord, error) {
	ts, err := time.Parse("2006-01-02 15:04:05", fields[1])
	if err != nil {
		ts = time.Now()
	}

	rcpt := ""
	if len(fields) > 3 {
		rcpt = fields[3]
	}
	domain := ""
	if idx := strings.LastIndex(rcpt, "@"); idx >= 0 {
		domain = strings.ToLower(rcpt[idx+1:])
	}

	rec := AcctRecord{
		Type:       fields[0],
		TimeLogged: ts,
		Domain:     domain,
		Rcpt:       rcpt,
	}
	if len(fields) > 2 {
		rec.Orig = fields[2]
	}
	return rec, nil
}

// AggregateByIP groups accounting records by source IP and computes rates.
func AggregateByIP(records []AcctRecord) map[string]*IPHealth {
	byIP := make(map[string]*IPHealth)

	for _, r := range records {
		ip := r.SourceIP
		if ip == "" {
			ip = "unknown"
		}

		h, ok := byIP[ip]
		if !ok {
			h = &IPHealth{IP: ip, Hostname: r.VMTA}
			byIP[ip] = h
		}

		switch r.Type {
		case "d":
			h.TotalDelivered++
			h.TotalSent++
		case "b", "rb":
			h.TotalBounced++
			h.TotalSent++
		case "f":
			h.TotalComplained++
		}
	}

	for _, h := range byIP {
		if h.TotalSent > 0 {
			h.DeliveryRate = float64(h.TotalDelivered) / float64(h.TotalSent) * 100
			h.BounceRate = float64(h.TotalBounced) / float64(h.TotalSent) * 100
		}
		if h.TotalDelivered > 0 {
			h.ComplaintRate = float64(h.TotalComplained) / float64(h.TotalDelivered) * 100
		}

		h.Status = "healthy"
		if h.BounceRate > 5.0 || h.ComplaintRate > 0.1 {
			h.Status = "critical"
		} else if h.BounceRate > 2.0 || h.ComplaintRate > 0.05 {
			h.Status = "warning"
		}

		h.LastChecked = time.Now()
	}

	return byIP
}

// AggregateByDomain groups accounting records by recipient domain.
func AggregateByDomain(records []AcctRecord) map[string]*DomainStatus {
	byDomain := make(map[string]*DomainStatus)

	for _, r := range records {
		d := r.Domain
		if d == "" {
			continue
		}

		ds, ok := byDomain[d]
		if !ok {
			ds = &DomainStatus{Domain: d}
			byDomain[d] = ds
		}

		switch r.Type {
		case "d":
			ds.Delivered++
		case "b", "rb":
			ds.Bounced++
		}
	}

	for _, ds := range byDomain {
		total := ds.Delivered + ds.Bounced
		if total > 0 {
			ds.DeliveryRate = float64(ds.Delivered) / float64(total) * 100
		}
	}

	return byDomain
}
