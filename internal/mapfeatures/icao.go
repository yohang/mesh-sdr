package mapfeatures

import (
	"slices"
	"strconv"
	"strings"
)

// ICAO 24-bit addresses (AIR-007): the state of registry from the
// allocation table of ICAO Annex 10 Volume III (Part I, Appendix to
// Chapter 9), and the registration of the states whose addresses follow
// a regular pattern of their registrations. The patterns were worked out
// from the national registers (the dump1090 and tar1090 projects publish
// the same ones); a registration out of a pattern stays unknown.

// icaoBlock is an address block of a state: its range, ISO 3166 alpha-2
// code ("" for the ICAO blocks) and name.
type icaoBlock struct {
	lo, hi uint32
	iso    string
	name   string
}

// icaoBlocks are the allocated blocks, by address.
var icaoBlocks = []icaoBlock{
	{0x004000, 0x0043FF, "ZW", "Zimbabwe"},
	{0x006000, 0x006FFF, "MZ", "Mozambique"},
	{0x008000, 0x00FFFF, "ZA", "South Africa"},
	{0x010000, 0x017FFF, "EG", "Egypt"},
	{0x018000, 0x01FFFF, "LY", "Libya"},
	{0x020000, 0x027FFF, "MA", "Morocco"},
	{0x028000, 0x02FFFF, "TN", "Tunisia"},
	{0x030000, 0x0303FF, "BW", "Botswana"},
	{0x032000, 0x032FFF, "BI", "Burundi"},
	{0x034000, 0x034FFF, "CM", "Cameroon"},
	{0x035000, 0x0353FF, "KM", "Comoros"},
	{0x036000, 0x036FFF, "CG", "Congo"},
	{0x038000, 0x038FFF, "CI", "Côte d'Ivoire"},
	{0x03E000, 0x03EFFF, "GA", "Gabon"},
	{0x040000, 0x040FFF, "ET", "Ethiopia"},
	{0x042000, 0x042FFF, "GQ", "Equatorial Guinea"},
	{0x044000, 0x044FFF, "GH", "Ghana"},
	{0x046000, 0x046FFF, "GN", "Guinea"},
	{0x048000, 0x0483FF, "GW", "Guinea-Bissau"},
	{0x04A000, 0x04A3FF, "LS", "Lesotho"},
	{0x04C000, 0x04CFFF, "KE", "Kenya"},
	{0x050000, 0x050FFF, "LR", "Liberia"},
	{0x054000, 0x054FFF, "MG", "Madagascar"},
	{0x058000, 0x058FFF, "MW", "Malawi"},
	{0x05A000, 0x05A3FF, "MV", "Maldives"},
	{0x05C000, 0x05CFFF, "ML", "Mali"},
	{0x05E000, 0x05E3FF, "MR", "Mauritania"},
	{0x060000, 0x0603FF, "MU", "Mauritius"},
	{0x062000, 0x062FFF, "NE", "Niger"},
	{0x064000, 0x064FFF, "NG", "Nigeria"},
	{0x068000, 0x068FFF, "UG", "Uganda"},
	{0x06A000, 0x06A3FF, "QA", "Qatar"},
	{0x06C000, 0x06CFFF, "CF", "Central African Republic"},
	{0x06E000, 0x06EFFF, "RW", "Rwanda"},
	{0x070000, 0x070FFF, "SN", "Senegal"},
	{0x074000, 0x0743FF, "SC", "Seychelles"},
	{0x076000, 0x0763FF, "SL", "Sierra Leone"},
	{0x078000, 0x078FFF, "SO", "Somalia"},
	{0x07A000, 0x07A3FF, "SZ", "Eswatini"},
	{0x07C000, 0x07CFFF, "SD", "Sudan"},
	{0x080000, 0x080FFF, "TZ", "Tanzania"},
	{0x084000, 0x084FFF, "TD", "Chad"},
	{0x088000, 0x088FFF, "TG", "Togo"},
	{0x08A000, 0x08AFFF, "ZM", "Zambia"},
	{0x08C000, 0x08CFFF, "CD", "DR Congo"},
	{0x090000, 0x090FFF, "AO", "Angola"},
	{0x094000, 0x0943FF, "BJ", "Benin"},
	{0x096000, 0x0963FF, "CV", "Cabo Verde"},
	{0x098000, 0x0983FF, "DJ", "Djibouti"},
	{0x09A000, 0x09AFFF, "GM", "Gambia"},
	{0x09C000, 0x09CFFF, "BF", "Burkina Faso"},
	{0x09E000, 0x09E3FF, "ST", "São Tomé and Príncipe"},
	{0x0A0000, 0x0A7FFF, "DZ", "Algeria"},
	{0x0A8000, 0x0A8FFF, "BS", "Bahamas"},
	{0x0AA000, 0x0AA3FF, "BB", "Barbados"},
	{0x0AB000, 0x0AB3FF, "BZ", "Belize"},
	{0x0AC000, 0x0ACFFF, "CO", "Colombia"},
	{0x0AE000, 0x0AEFFF, "CR", "Costa Rica"},
	{0x0B0000, 0x0B0FFF, "CU", "Cuba"},
	{0x0B2000, 0x0B2FFF, "SV", "El Salvador"},
	{0x0B4000, 0x0B4FFF, "GT", "Guatemala"},
	{0x0B6000, 0x0B6FFF, "GY", "Guyana"},
	{0x0B8000, 0x0B8FFF, "HT", "Haiti"},
	{0x0BA000, 0x0BAFFF, "HN", "Honduras"},
	{0x0BC000, 0x0BC3FF, "VC", "Saint Vincent and the Grenadines"},
	{0x0BE000, 0x0BEFFF, "JM", "Jamaica"},
	{0x0C0000, 0x0C0FFF, "NI", "Nicaragua"},
	{0x0C2000, 0x0C2FFF, "PA", "Panama"},
	{0x0C4000, 0x0C4FFF, "DO", "Dominican Republic"},
	{0x0C6000, 0x0C6FFF, "TT", "Trinidad and Tobago"},
	{0x0C8000, 0x0C8FFF, "SR", "Suriname"},
	{0x0CA000, 0x0CA3FF, "AG", "Antigua and Barbuda"},
	{0x0CC000, 0x0CC3FF, "GD", "Grenada"},
	{0x0D0000, 0x0D7FFF, "MX", "Mexico"},
	{0x0D8000, 0x0DFFFF, "VE", "Venezuela"},
	{0x100000, 0x1FFFFF, "RU", "Russia"},
	{0x201000, 0x2013FF, "NA", "Namibia"},
	{0x202000, 0x2023FF, "ER", "Eritrea"},
	{0x300000, 0x33FFFF, "IT", "Italy"},
	{0x340000, 0x37FFFF, "ES", "Spain"},
	{0x380000, 0x3BFFFF, "FR", "France"},
	{0x3C0000, 0x3FFFFF, "DE", "Germany"},
	{0x400000, 0x43FFFF, "GB", "United Kingdom"},
	{0x440000, 0x447FFF, "AT", "Austria"},
	{0x448000, 0x44FFFF, "BE", "Belgium"},
	{0x450000, 0x457FFF, "BG", "Bulgaria"},
	{0x458000, 0x45FFFF, "DK", "Denmark"},
	{0x460000, 0x467FFF, "FI", "Finland"},
	{0x468000, 0x46FFFF, "GR", "Greece"},
	{0x470000, 0x477FFF, "HU", "Hungary"},
	{0x478000, 0x47FFFF, "NO", "Norway"},
	{0x480000, 0x487FFF, "NL", "Netherlands"},
	{0x488000, 0x48FFFF, "PL", "Poland"},
	{0x490000, 0x497FFF, "PT", "Portugal"},
	{0x498000, 0x49FFFF, "CZ", "Czechia"},
	{0x4A0000, 0x4A7FFF, "RO", "Romania"},
	{0x4A8000, 0x4AFFFF, "SE", "Sweden"},
	{0x4B0000, 0x4B7FFF, "CH", "Switzerland"},
	{0x4B8000, 0x4BFFFF, "TR", "Türkiye"},
	{0x4C0000, 0x4C7FFF, "RS", "Serbia"},
	{0x4C8000, 0x4C83FF, "CY", "Cyprus"},
	{0x4CA000, 0x4CAFFF, "IE", "Ireland"},
	{0x4CC000, 0x4CCFFF, "IS", "Iceland"},
	{0x4D0000, 0x4D03FF, "LU", "Luxembourg"},
	{0x4D2000, 0x4D23FF, "MT", "Malta"},
	{0x4D4000, 0x4D43FF, "MC", "Monaco"},
	{0x500000, 0x5003FF, "SM", "San Marino"},
	{0x501000, 0x5013FF, "AL", "Albania"},
	{0x501C00, 0x501FFF, "HR", "Croatia"},
	{0x502C00, 0x502FFF, "LV", "Latvia"},
	{0x503C00, 0x503FFF, "LT", "Lithuania"},
	{0x504C00, 0x504FFF, "MD", "Moldova"},
	{0x505C00, 0x505FFF, "SK", "Slovakia"},
	{0x506C00, 0x506FFF, "SI", "Slovenia"},
	{0x507C00, 0x507FFF, "UZ", "Uzbekistan"},
	{0x508000, 0x50FFFF, "UA", "Ukraine"},
	{0x510000, 0x5103FF, "BY", "Belarus"},
	{0x511000, 0x5113FF, "EE", "Estonia"},
	{0x512000, 0x5123FF, "MK", "North Macedonia"},
	{0x513000, 0x5133FF, "BA", "Bosnia and Herzegovina"},
	{0x514000, 0x5143FF, "GE", "Georgia"},
	{0x515000, 0x5153FF, "TJ", "Tajikistan"},
	{0x516000, 0x5163FF, "ME", "Montenegro"},
	{0x600000, 0x6003FF, "AM", "Armenia"},
	{0x600800, 0x600BFF, "AZ", "Azerbaijan"},
	{0x601000, 0x6013FF, "KG", "Kyrgyzstan"},
	{0x601800, 0x601BFF, "TM", "Turkmenistan"},
	{0x680000, 0x6803FF, "BT", "Bhutan"},
	{0x681000, 0x6813FF, "FM", "Micronesia"},
	{0x682000, 0x6823FF, "MN", "Mongolia"},
	{0x683000, 0x6833FF, "KZ", "Kazakhstan"},
	{0x684000, 0x6843FF, "PW", "Palau"},
	{0x700000, 0x700FFF, "AF", "Afghanistan"},
	{0x702000, 0x702FFF, "BD", "Bangladesh"},
	{0x704000, 0x704FFF, "MM", "Myanmar"},
	{0x706000, 0x706FFF, "KW", "Kuwait"},
	{0x708000, 0x708FFF, "LA", "Laos"},
	{0x70A000, 0x70AFFF, "NP", "Nepal"},
	{0x70C000, 0x70C3FF, "OM", "Oman"},
	{0x70E000, 0x70EFFF, "KH", "Cambodia"},
	{0x710000, 0x717FFF, "SA", "Saudi Arabia"},
	{0x718000, 0x71FFFF, "KR", "South Korea"},
	{0x720000, 0x727FFF, "KP", "North Korea"},
	{0x728000, 0x72FFFF, "IQ", "Iraq"},
	{0x730000, 0x737FFF, "IR", "Iran"},
	{0x738000, 0x73FFFF, "IL", "Israel"},
	{0x740000, 0x747FFF, "JO", "Jordan"},
	{0x748000, 0x74FFFF, "LB", "Lebanon"},
	{0x750000, 0x757FFF, "MY", "Malaysia"},
	{0x758000, 0x75FFFF, "PH", "Philippines"},
	{0x760000, 0x767FFF, "PK", "Pakistan"},
	{0x768000, 0x76FFFF, "SG", "Singapore"},
	{0x770000, 0x777FFF, "LK", "Sri Lanka"},
	{0x778000, 0x77FFFF, "SY", "Syria"},
	{0x780000, 0x7BFFFF, "CN", "China"},
	{0x7C0000, 0x7FFFFF, "AU", "Australia"},
	{0x800000, 0x83FFFF, "IN", "India"},
	{0x840000, 0x87FFFF, "JP", "Japan"},
	{0x880000, 0x887FFF, "TH", "Thailand"},
	{0x888000, 0x88FFFF, "VN", "Viet Nam"},
	{0x890000, 0x890FFF, "YE", "Yemen"},
	{0x894000, 0x894FFF, "BH", "Bahrain"},
	{0x895000, 0x8953FF, "BN", "Brunei"},
	{0x896000, 0x896FFF, "AE", "United Arab Emirates"},
	{0x897000, 0x8973FF, "SB", "Solomon Islands"},
	{0x898000, 0x898FFF, "PG", "Papua New Guinea"},
	{0x899000, 0x8993FF, "", "ICAO (special use)"},
	{0x8A0000, 0x8A7FFF, "ID", "Indonesia"},
	{0x900000, 0x9003FF, "MH", "Marshall Islands"},
	{0x901000, 0x9013FF, "CK", "Cook Islands"},
	{0x902000, 0x9023FF, "WS", "Samoa"},
	{0xA00000, 0xAFFFFF, "US", "United States"},
	{0xC00000, 0xC3FFFF, "CA", "Canada"},
	{0xC80000, 0xC87FFF, "NZ", "New Zealand"},
	{0xC88000, 0xC88FFF, "FJ", "Fiji"},
	{0xC8A000, 0xC8A3FF, "NR", "Nauru"},
	{0xC8C000, 0xC8C3FF, "LC", "Saint Lucia"},
	{0xC8D000, 0xC8D3FF, "TO", "Tonga"},
	{0xC8E000, 0xC8E3FF, "KI", "Kiribati"},
	{0xC90000, 0xC903FF, "VU", "Vanuatu"},
	{0xE00000, 0xE3FFFF, "AR", "Argentina"},
	{0xE40000, 0xE7FFFF, "BR", "Brazil"},
	{0xE80000, 0xE80FFF, "CL", "Chile"},
	{0xE84000, 0xE84FFF, "EC", "Ecuador"},
	{0xE88000, 0xE88FFF, "PY", "Paraguay"},
	{0xE8C000, 0xE8CFFF, "PE", "Peru"},
	{0xE90000, 0xE90FFF, "UY", "Uruguay"},
	{0xE94000, 0xE94FFF, "BO", "Bolivia"},
	{0xF00000, 0xF07FFF, "", "ICAO (temporary)"},
	{0xF09000, 0xF093FF, "", "ICAO (special use)"},
}

// ParseICAO parses a 24-bit address in hex (a leading "~", a non-ICAO
// address, is not one).
func ParseICAO(s string) (uint32, bool) {
	if len(s) != 6 {
		return 0, false
	}

	v, err := strconv.ParseUint(s, 16, 32)

	return uint32(v), err == nil
}

// ICAOCountry returns the state of registry of an address: its ISO 3166
// alpha-2 code ("" for an ICAO block) and name; ok is false for an
// unallocated address.
func ICAOCountry(addr uint32) (iso, name string, ok bool) {
	i, found := slices.BinarySearchFunc(icaoBlocks, addr, func(b icaoBlock, a uint32) int {
		switch {
		case b.hi < a:
			return -1
		case b.lo > a:
			return 1
		default:
			return 0
		}
	})
	if !found {
		return "", "", false
	}

	return icaoBlocks[i].iso, icaoBlocks[i].name, true
}

// ICAORegistration returns the registration of an address of a state that
// derives addresses from registrations ("" otherwise): the United States
// (N-numbers), Japan (JA), South Korea (HL), Russia (RA- numeric), Cuba
// (CU-T), and the three-letter suffixes of Canada, Australia, Germany,
// France, South Africa and other states.
func ICAORegistration(addr uint32) string {
	for _, f := range []func(uint32) string{usRegistration, japanRegistration, koreaRegistration, numericRegistration, strideRegistration} {
		if r := f(addr); r != "" {
			return r
		}
	}

	return ""
}

// Registration alphabets: the US and Japanese ones leave out I and O.
const (
	limitedLetters = "ABCDEFGHJKLMNPQRSTUVWXYZ"
	allLetters     = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
)

// US N-numbers: A00001 (N1) to ADF7C7 (N99999), in registry order. After
// each digit come the suffixes "", "A".."Z", "AA".."ZZ" (601), then the
// next digit's blocks; the fifth character is a letter or a digit.
const (
	usFirst  = 0xA00001
	usCount  = 915_399
	usDigit1 = 101_711
	usDigit2 = 10_111
	usDigit3 = 951
	usDigit4 = 35
	usLetter = 601
)

func usRegistration(addr uint32) string {
	if addr < usFirst || addr-usFirst >= usCount {
		return ""
	}

	off := int(addr - usFirst)

	var b strings.Builder

	b.WriteString("N")
	b.WriteString(strconv.Itoa(off/usDigit1 + 1))
	off %= usDigit1

	for _, size := range []int{usDigit2, usDigit3} {
		if off < usLetter {
			return b.String() + usSuffix(off)
		}

		off -= usLetter
		b.WriteString(strconv.Itoa(off / size))
		off %= size
	}

	if off < usLetter {
		return b.String() + usSuffix(off)
	}

	off -= usLetter
	b.WriteString(strconv.Itoa(off / usDigit4))
	off %= usDigit4

	switch {
	case off == 0:
	case off <= len(limitedLetters):
		b.WriteByte(limitedLetters[off-1])
	default:
		b.WriteString(strconv.Itoa(off - len(limitedLetters) - 1))
	}

	return b.String()
}

// usSuffix is the n-th letter suffix of an N-number: "", then each letter
// followed by its 24 two-letter suffixes.
func usSuffix(n int) string {
	if n == 0 {
		return ""
	}

	n--
	s := string(limitedLetters[n/25])

	if second := n % 25; second > 0 {
		s += string(limitedLetters[second-1])
	}

	return s
}

// Japan: JA followed by two digits, then two digits, a digit and a letter
// or two letters, from 840000.
const (
	japanFirst  = 0x840000
	japanDigit1 = 22_984
	japanDigit2 = 916
	japanDigits = 340
	japanDigit3 = 34
)

func japanRegistration(addr uint32) string {
	if addr < japanFirst || addr-japanFirst >= 10*japanDigit1 {
		return ""
	}

	off := int(addr - japanFirst)
	reg := "JA" + strconv.Itoa(off/japanDigit1)
	off %= japanDigit1

	d2 := off / japanDigit2
	if d2 > 9 {
		return ""
	}

	reg += strconv.Itoa(d2)
	off %= japanDigit2

	if off >= japanDigits {
		off -= japanDigits

		return reg + string(limitedLetters[off/24]) + string(limitedLetters[off%24])
	}

	reg += strconv.Itoa(off / japanDigit3)
	off %= japanDigit3

	if off < 10 {
		return reg + strconv.Itoa(off)
	}

	return reg + string(limitedLetters[off-10])
}

// South Korea: HL followed by the address offset written in hex, which
// gives the four registration digits (HL7200 to HL7799, HL8000 to HL8099,
// HL8200 to HL8299); an offset with a hex letter is no registration.
func koreaRegistration(addr uint32) string {
	for _, r := range []struct{ lo, hi, base uint32 }{
		{0x71BA00, 0x71BF99, 0x7200}, {0x71C000, 0x71C099, 0x8000}, {0x71C200, 0x71C299, 0x8200},
	} {
		if addr < r.lo || addr > r.hi {
			continue
		}

		if n := strconv.FormatUint(uint64(addr-r.lo+r.base), 16); strings.Trim(n, "0123456789") == "" {
			return "HL" + n
		}

		return ""
	}

	return ""
}

// numericBlock is a block of numeric registrations: the address of the
// first one, its number, how many, and the template whose trailing
// characters the number replaces.
type numericBlock struct {
	start, first, count uint32
	template            string
}

var numericBlocks = []numericBlock{
	{start: 0x140000, first: 0, count: 100_000, template: "RA-00000"},
	{start: 0x0B03E8, first: 1000, count: 1000, template: "CU-T0000"},
}

func numericRegistration(addr uint32) string {
	for _, b := range numericBlocks {
		if addr >= b.start && addr-b.start < b.count {
			n := strconv.FormatUint(uint64(addr-b.start+b.first), 10)

			return b.template[:len(b.template)-len(n)] + n
		}
	}

	return ""
}

// strideBlock maps a block of addresses to three-letter suffixes: the
// first letter changes every s1 addresses, the second every s2, the third
// every address (the strides leave gaps: an address in a gap is no
// registration). first and last bound the suffixes of the block ("": AAA
// to ZZZ).
type strideBlock struct {
	start       uint32
	s1, s2      int
	prefix      string
	first, last string
}

var strideBlocks = []strideBlock{
	{start: 0x008011, s1: 26 * 26, s2: 26, prefix: "ZS-"},
	{start: 0x390000, s1: 1024, s2: 32, prefix: "F-G"},
	{start: 0x398000, s1: 1024, s2: 32, prefix: "F-H"},
	{start: 0x3C4421, s1: 1024, s2: 32, prefix: "D-A", first: "AAA", last: "OZZ"},
	{start: 0x3C0001, s1: 26 * 26, s2: 26, prefix: "D-A", first: "PAA", last: "ZZZ"},
	{start: 0x3C8421, s1: 1024, s2: 32, prefix: "D-B", first: "AAA", last: "OZZ"},
	{start: 0x3C2001, s1: 26 * 26, s2: 26, prefix: "D-B", first: "PAA", last: "ZZZ"},
	{start: 0x3CC000, s1: 26 * 26, s2: 26, prefix: "D-C"},
	{start: 0x3D04A8, s1: 26 * 26, s2: 26, prefix: "D-E"},
	{start: 0x3D4950, s1: 26 * 26, s2: 26, prefix: "D-F"},
	{start: 0x3D8DF8, s1: 26 * 26, s2: 26, prefix: "D-G"},
	{start: 0x3DD2A0, s1: 26 * 26, s2: 26, prefix: "D-H"},
	{start: 0x3E1748, s1: 26 * 26, s2: 26, prefix: "D-I"},
	{start: 0x448421, s1: 1024, s2: 32, prefix: "OO-"},
	{start: 0x458421, s1: 1024, s2: 32, prefix: "OY-"},
	{start: 0x460000, s1: 26 * 26, s2: 26, prefix: "OH-"},
	{start: 0x468421, s1: 1024, s2: 32, prefix: "SX-"},
	{start: 0x490421, s1: 1024, s2: 32, prefix: "CS-"},
	{start: 0x4A0421, s1: 1024, s2: 32, prefix: "YR-"},
	{start: 0x4B8421, s1: 1024, s2: 32, prefix: "TC-"},
	{start: 0x740421, s1: 1024, s2: 32, prefix: "JY-"},
	{start: 0x760421, s1: 1024, s2: 32, prefix: "AP-"},
	{start: 0x768421, s1: 1024, s2: 32, prefix: "9V-"},
	{start: 0x778421, s1: 1024, s2: 32, prefix: "YK-"},
	{start: 0x7C0000, s1: 36 * 36, s2: 36, prefix: "VH-"},
	{start: 0xC00001, s1: 26 * 26, s2: 26, prefix: "C-F"},
	{start: 0xC044A9, s1: 26 * 26, s2: 26, prefix: "C-G"},
	{start: 0xE01041, s1: 4096, s2: 64, prefix: "LV-"},
}

// index returns the position of a suffix in the block's address pattern.
func (b strideBlock) index(suffix string) int {
	return strings.IndexByte(allLetters, suffix[0])*b.s1 + strings.IndexByte(allLetters, suffix[1])*b.s2 + strings.IndexByte(allLetters, suffix[2])
}

func strideRegistration(addr uint32) string {
	for _, b := range strideBlocks {
		first, last := b.first, b.last
		if first == "" {
			first, last = "AAA", "ZZZ"
		}

		base := b.index(first)
		if addr < b.start || int(addr-b.start) > b.index(last)-base {
			continue
		}

		off := int(addr-b.start) + base
		i1, i2, i3 := off/b.s1, off%b.s1/b.s2, off%b.s2

		if i1 >= len(allLetters) || i2 >= len(allLetters) || i3 >= len(allLetters) {
			continue
		}

		return b.prefix + string(allLetters[i1]) + string(allLetters[i2]) + string(allLetters[i3])
	}

	return ""
}
