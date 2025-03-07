package desec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/StackExchange/dnscontrol/v4/models"
	"github.com/StackExchange/dnscontrol/v4/pkg/diff"
	"github.com/StackExchange/dnscontrol/v4/pkg/diff2"
	"github.com/StackExchange/dnscontrol/v4/pkg/printer"
	"github.com/StackExchange/dnscontrol/v4/pkg/txtutil"
	"github.com/StackExchange/dnscontrol/v4/providers"
	"github.com/miekg/dns/dnsutil"
)

/*
desec API DNS provider:
Info required in `creds.json`:
   - auth-token
*/

// NewDeSec creates the provider.
func NewDeSec(m map[string]string, metadata json.RawMessage) (providers.DNSServiceProvider, error) {
	c := &desecProvider{}
	c.creds.token = m["auth-token"]
	if c.creds.token == "" {
		return nil, fmt.Errorf("missing deSEC auth-token")
	}
	if err := c.authenticate(); err != nil {
		return nil, fmt.Errorf("authentication failed")
	}
	//DomainIndex is used for corrections (minttl) and domain creation
	if err := c.initializeDomainIndex(); err != nil {
		return nil, err
	}

	return c, nil
}

var features = providers.DocumentationNotes{
	providers.CanAutoDNSSEC:          providers.Can("deSEC always signs all records. When trying to disable, a notice is printed."),
	providers.CanGetZones:            providers.Can(),
	providers.CanUseAlias:            providers.Unimplemented("Apex aliasing is supported via new SVCB and HTTPS record types. For details, check the deSEC docs."),
	providers.CanUseCAA:              providers.Can(),
	providers.CanUseDS:               providers.Can(),
	providers.CanUseLOC:              providers.Unimplemented(),
	providers.CanUseNAPTR:            providers.Can(),
	providers.CanUsePTR:              providers.Can(),
	providers.CanUseSRV:              providers.Can(),
	providers.CanUseSSHFP:            providers.Can(),
	providers.CanUseTLSA:             providers.Can(),
	providers.DocCreateDomains:       providers.Can(),
	providers.DocDualHost:            providers.Unimplemented(),
	providers.DocOfficiallySupported: providers.Cannot(),
}

var defaultNameServerNames = []string{
	"ns1.desec.io",
	"ns2.desec.org",
}

func init() {
	fns := providers.DspFuncs{
		Initializer:   NewDeSec,
		RecordAuditor: AuditRecords,
	}
	providers.RegisterDomainServiceProviderType("DESEC", fns, features)
}

// GetNameservers returns the nameservers for a domain.
func (c *desecProvider) GetNameservers(domain string) ([]*models.Nameserver, error) {
	return models.ToNameservers(defaultNameServerNames)
}

// func (c *desecProvider) GetDomainCorrections(dc *models.DomainConfig) ([]*models.Correction, error) {
// 	if dc.AutoDNSSEC == "off" {
// 		printer.Printf("Notice: DNSSEC signing was not requested, but cannot be turned off. (deSEC always signs all records.)\n")
// 	}

// 	existing, err := c.GetZoneRecords(dc.Name)
// 	if err != nil {
// 		return nil, err
// 	}
// 	models.PostProcessRecords(existing)
// 	clean := PrepFoundRecords(existing)
// 	var minTTL uint32
// 	c.mutex.Lock()
// 	if ttl, ok := c.domainIndex[dc.Name]; !ok {
// 		minTTL = 3600
// 	} else {
// 		minTTL = ttl
// 	}
// 	c.mutex.Unlock()
// 	PrepDesiredRecords(dc, minTTL)
// 	return c.GetZoneRecordsCorrections(dc, clean)
// }

// GetZoneRecords gets the records of a zone and returns them in RecordConfig format.
func (c *desecProvider) GetZoneRecords(domain string, meta map[string]string) (models.Records, error) {
	records, err := c.getRecords(domain)
	if err != nil {
		return nil, err
	}

	// Convert them to DNScontrol's native format:
	existingRecords := []*models.RecordConfig{}
	//spew.Dump(records)
	for _, rr := range records {
		existingRecords = append(existingRecords, nativeToRecords(rr, domain)...)
	}

	return existingRecords, nil
}

// EnsureZoneExists creates a zone if it does not exist
func (c *desecProvider) EnsureZoneExists(domain string) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if _, ok := c.domainIndex[domain]; ok {
		return nil
	}
	return c.createDomain(domain)
}

// PrepDesiredRecords munges any records to best suit this provider.
func PrepDesiredRecords(dc *models.DomainConfig, minTTL uint32) {
	// Sort through the dc.Records, eliminate any that can't be
	// supported; modify any that need adjustments to work with the
	// provider.  We try to do minimal changes otherwise it gets
	// confusing.

	//dc.Punycode()
	recordsToKeep := make([]*models.RecordConfig, 0, len(dc.Records))
	for _, rec := range dc.Records {
		if rec.Type == "ALIAS" {
			// deSEC does not permit ALIAS records, just ignore it
			printer.Warnf("deSEC does not support alias records\n")
			continue
		}
		if rec.TTL < minTTL {
			if rec.Type != "NS" {
				printer.Warnf("Please contact support@desec.io if you need TTLs < %d. Setting TTL of %s type %s from %d to %d\n", minTTL, rec.GetLabelFQDN(), rec.Type, rec.TTL, minTTL)
			}
			rec.TTL = minTTL
		}
		recordsToKeep = append(recordsToKeep, rec)
	}
	dc.Records = recordsToKeep
}

// GetZoneRecordsCorrections returns a list of corrections that will turn existing records into dc.Records.
func (c *desecProvider) GetZoneRecordsCorrections(dc *models.DomainConfig, existing models.Records) ([]*models.Correction, error) {
	txtutil.SplitSingleLongTxt(dc.Records)

	var minTTL uint32
	c.mutex.Lock()
	if ttl, ok := c.domainIndex[dc.Name]; !ok {
		minTTL = 3600
	} else {
		minTTL = ttl
	}
	c.mutex.Unlock()
	PrepDesiredRecords(dc, minTTL)

	var corrections []*models.Correction
	var err error
	var keysToUpdate map[models.RecordKey][]string
	if !diff2.EnableDiff2 {
		// diff existing vs. current.
		keysToUpdate, err = (diff.New(dc)).ChangedGroups(existing)
	} else {
		keysToUpdate, err = (diff.NewCompat(dc)).ChangedGroups(existing)
	}
	if err != nil {
		return nil, err
	}

	if len(keysToUpdate) == 0 {
		return nil, nil
	}

	desiredRecords := dc.Records.GroupedByKey()
	var rrs []resourceRecord
	buf := &bytes.Buffer{}
	// For any key with an update, delete or replace those records.
	for label := range keysToUpdate {
		if _, ok := desiredRecords[label]; !ok {
			//we could not find this RecordKey in the desiredRecords
			//this means it must be deleted
			for i, msg := range keysToUpdate[label] {
				if i == 0 {
					rc := resourceRecord{}
					rc.Type = label.Type
					rc.Records = make([]string, 0) // empty array of records should delete this rrset
					rc.TTL = 3600
					shortname := dnsutil.TrimDomainName(label.NameFQDN, dc.Name)
					if shortname == "@" {
						shortname = ""
					}
					rc.Subname = shortname
					fmt.Fprintln(buf, msg)
					rrs = append(rrs, rc)
				} else {
					//just add the message
					fmt.Fprintln(buf, msg)
				}
			}
		} else {
			//it must be an update or create, both can be done with the same api call.
			ns := recordsToNative(desiredRecords[label], dc.Name)
			if len(ns) > 1 {
				panic("we got more than one resource record to create / modify")
			}
			for i, msg := range keysToUpdate[label] {
				if i == 0 {
					rrs = append(rrs, ns[0])
					fmt.Fprintln(buf, msg)
				} else {
					//noop just for printing the additional messages
					fmt.Fprintln(buf, msg)
				}
			}
		}
	}
	msg := fmt.Sprintf("Changes:\n%s", buf)
	corrections = append(corrections,
		&models.Correction{
			Msg: msg,
			F: func() error {
				rc := rrs
				err := c.upsertRR(rc, dc.Name)
				if err != nil {
					return err
				}
				return nil
			},
		})

	// NB(tlim): This sort is just to make updates look pretty. It is
	// cosmetic.  The risk here is that there may be some updates that
	// require a specific order (for example a delete before an add).
	// However the code doesn't seem to have such situation.  All tests
	// pass.  That said, if this breaks anything, the easiest fix might
	// be to just remove the sort.
	sort.Slice(corrections, func(i, j int) bool { return diff.CorrectionLess(corrections, i, j) })

	return corrections, nil
}

// ListZones return all the zones in the account
func (c *desecProvider) ListZones() ([]string, error) {
	var domains []string
	for domain := range c.domainIndex {
		domains = append(domains, domain)
	}
	return domains, nil
}
