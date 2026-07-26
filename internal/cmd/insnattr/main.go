package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

type generator struct {
	output            *os.File
	supportsInvalid64 bool

	table    map[string]string
	lptable1 map[string]string
	lptable2 map[string]string
	lptable3 map[string]string

	escape map[string]int
	group  map[string]int

	etable   map[pair]string
	gtable   map[pair]string
	atable   map[pair]string
	xoptable map[int]string

	eid   int
	gid   int
	aid   int
	xopid int
	tname string

	ggid   int
	geid   int
	gaid   int
	gxopid int
	line   int
}

type pair struct {
	a int
	b int
}

var (
	opndExpr        = regexp.MustCompile(`^[A-Za-z/]`)
	extExpr         = regexp.MustCompile(`^\(`)
	sepExpr         = regexp.MustCompile(`^\|$`)
	groupExpr       = regexp.MustCompile(`^Grp[0-9A-Za-z]+`)
	immExpr         = regexp.MustCompile(`^[IJAOL][a-z]`)
	modrmExpr       = regexp.MustCompile(`^([CDEGMNPQRSUVW/][a-z]+|NTA|T[012])`)
	force64Expr     = regexp.MustCompile(`\([df]64\)`)
	invalid64Expr   = regexp.MustCompile(`\(i64\)`)
	only64Expr      = regexp.MustCompile(`\(o64\)`)
	rexExpr         = regexp.MustCompile(`^((REX(\.[XRWB]+)+)|(REX$))`)
	rex2Expr        = regexp.MustCompile(`\(REX2\)`)
	noRex2Expr      = regexp.MustCompile(`\(!REX2\)`)
	fpuExpr         = regexp.MustCompile(`^ESC`)
	lprefix1Expr    = regexp.MustCompile(`\((66|!F3)\)`)
	lprefix2Expr    = regexp.MustCompile(`\(F3\)`)
	lprefix3Expr    = regexp.MustCompile(`\((F2|!F3|66&F2)\)`)
	lprefixExpr     = regexp.MustCompile(`\((66|F2|F3)\)`)
	vexOKOpcodeExpr = regexp.MustCompile(`^[vk].*`)
	vexOKExpr       = regexp.MustCompile(`\(v1\)`)
	vexOnlyExpr     = regexp.MustCompile(`\(v\)`)
	evexOnlyExpr    = regexp.MustCompile(`\(ev\)`)
	evexScalable    = regexp.MustCompile(`\(es\)`)
	xopOKExpr       = regexp.MustCompile(`\(xop\)`)
	prefixExpr      = regexp.MustCompile(`\(Prefix\)`)
	opcodeExpr      = regexp.MustCompile(`^[0-9a-f]+:`)
)

var immFlag = map[string]string{
	"Ib": "INAT_MAKE_IMM(INAT_IMM_BYTE)",
	"Jb": "INAT_MAKE_IMM(INAT_IMM_BYTE)",
	"Iw": "INAT_MAKE_IMM(INAT_IMM_WORD)",
	"Id": "INAT_MAKE_IMM(INAT_IMM_DWORD)",
	"Iq": "INAT_MAKE_IMM(INAT_IMM_QWORD)",
	"Ap": "INAT_MAKE_IMM(INAT_IMM_PTR)",
	"Iz": "INAT_MAKE_IMM(INAT_IMM_VWORD32)",
	"Jz": "INAT_MAKE_IMM(INAT_IMM_VWORD32)",
	"Iv": "INAT_MAKE_IMM(INAT_IMM_VWORD)",
	"Ob": "INAT_MOFFSET",
	"Ov": "INAT_MOFFSET",
	"Lx": "INAT_MAKE_IMM(INAT_IMM_BYTE)",
	"Lo": "INAT_MAKE_IMM(INAT_IMM_BYTE)",
}

var prefixNum = map[string]string{
	"Operand-Size": "INAT_PFX_OPNDSZ",
	"REPNE":        "INAT_PFX_REPNE",
	"REP/REPE":     "INAT_PFX_REPE",
	"XACQUIRE":     "INAT_PFX_REPNE",
	"XRELEASE":     "INAT_PFX_REPE",
	"LOCK":         "INAT_PFX_LOCK",
	"SEG=CS":       "INAT_PFX_CS",
	"SEG=DS":       "INAT_PFX_DS",
	"SEG=ES":       "INAT_PFX_ES",
	"SEG=FS":       "INAT_PFX_FS",
	"SEG=GS":       "INAT_PFX_GS",
	"SEG=SS":       "INAT_PFX_SS",
	"Address-Size": "INAT_PFX_ADDRSZ",
	"VEX+1byte":    "INAT_PFX_VEX2",
	"VEX+2byte":    "INAT_PFX_VEX3",
	"EVEX":         "INAT_PFX_EVEX",
	"REX2":         "INAT_PFX_REX2",
	"XOP":          "INAT_PFX_XOP",
}

func main() {
	in := flag.String("in", "", "arch/x86/lib/x86-opcode-map.txt input")
	inatH := flag.String("inat_h", "", "tools/arch/x86/include/asm/inat.h input")
	out := flag.String("out", "", "Generated inat-tables.c output")
	flag.Parse()

	if *in == "" || *inatH == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "-in, -inat_h, and -out are required")
		os.Exit(2)
	}
	supportsInvalid64, err := headerDefines(*inatH, "INAT_INV64")
	if err != nil {
		fmt.Fprintf(os.Stderr, "insnattr: %v\n", err)
		os.Exit(1)
	}
	if err := run(*in, *out, supportsInvalid64); err != nil {
		fmt.Fprintf(os.Stderr, "insnattr: %v\n", err)
		os.Exit(1)
	}
}

func headerDefines(path, name string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	return regexp.MustCompile(`(?m)^[[:space:]]*#define[[:space:]]+` + regexp.QuoteMeta(name) + `(?:[[:space:]]|$)`).Match(data), nil
}

func run(in, out string, supportsInvalid64 bool) error {
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	output, err := os.Create(out)
	if err != nil {
		return err
	}
	defer output.Close()

	g := newGenerator(output, supportsInvalid64)
	if err := g.process(in); err != nil {
		return err
	}
	g.finish()
	return nil
}

func newGenerator(output *os.File, supportsInvalid64 bool) *generator {
	g := &generator{
		output:            output,
		supportsInvalid64: supportsInvalid64,
		escape:            map[string]int{},
		group:             map[string]int{},
		etable:            map[pair]string{},
		gtable:            map[pair]string{},
		atable:            map[pair]string{},
		xoptable:          map[int]string{},
		ggid:              1,
		geid:              1,
	}
	g.clearVars()
	fmt.Fprint(output, "/* x86 opcode map generated from x86-opcode-map.txt */\n")
	fmt.Fprint(output, "/* Do not change this code. */\n\n")
	return g
}

func (g *generator) clearVars() {
	g.table = map[string]string{}
	g.lptable1 = map[string]string{}
	g.lptable2 = map[string]string{}
	g.lptable3 = map[string]string{}
	g.eid = -1
	g.gid = -1
	g.aid = -1
	g.xopid = -1
	g.tname = ""
}

func (g *generator) process(path string) error {
	input, err := os.Open(path)
	if err != nil {
		return err
	}
	defer input.Close()

	scanner := bufio.NewScanner(input)
	for scanner.Scan() {
		g.line++
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch {
		case strings.HasPrefix(line, "Table:"):
			if g.tname != "" {
				return g.semanticError("Hit Table: before EndTable:.")
			}
			fmt.Fprintf(g.output, "/* %s */\n", line)
		case strings.HasPrefix(line, "Referrer:"):
			if len(fields) != 1 {
				ref := strings.Join(fields[1:], "")
				g.eid = g.escape[ref]
				g.tname = fmt.Sprintf("inat_escape_table_%d", g.eid)
			}
		case strings.HasPrefix(line, "AVXcode:"):
			if len(fields) != 1 {
				aid, err := strconv.Atoi(fields[1])
				if err != nil {
					return g.semanticError("Bad AVXcode: " + fields[1])
				}
				g.aid = aid
				g.xopid = -1
				if g.gaid <= aid {
					g.gaid = aid + 1
				}
				if g.tname == "" {
					g.tname = fmt.Sprintf("inat_avx_table_%d", aid)
				}
			}
			if g.aid == -1 && g.eid == -1 {
				g.tname = "inat_primary_table"
			}
		case strings.HasPrefix(line, "XOPcode:"):
			if len(fields) != 1 {
				xopid, err := strconv.Atoi(fields[1])
				if err != nil {
					return g.semanticError("Bad XOPcode: " + fields[1])
				}
				g.xopid = xopid
				g.aid = -1
				if g.gxopid <= xopid {
					g.gxopid = xopid + 1
				}
				if g.tname == "" {
					g.tname = fmt.Sprintf("inat_xop_table_%d", xopid)
				}
			}
			if g.xopid == -1 && g.eid == -1 {
				g.tname = "inat_primary_table"
			}
		case strings.HasPrefix(line, "GrpTable:"):
			fmt.Fprintf(g.output, "/* %s */\n", line)
			id, ok := g.group[fields[1]]
			if !ok {
				return g.semanticError("No group: " + fields[1])
			}
			g.gid = id
			g.tname = fmt.Sprintf("inat_group_table_%d", id)
		case strings.HasPrefix(line, "EndTable"):
			g.endTable()
		case opcodeExpr.MatchString(line):
			if err := g.opcodeLine(line, fields); err != nil {
				return err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func (g *generator) semanticError(message string) error {
	return fmt.Errorf("semantic error at %d: %s", g.line, message)
}

func (g *generator) endTable() {
	if g.gid != -1 {
		if len(g.table) != 0 {
			g.printTable(g.table, fmt.Sprintf("%s[INAT_GROUP_TABLE_SIZE]", g.tname), "0x%x", 8)
			g.gtable[pair{g.gid, 0}] = g.tname
		}
		if len(g.lptable1) != 0 {
			g.printTable(g.lptable1, fmt.Sprintf("%s_1[INAT_GROUP_TABLE_SIZE]", g.tname), "0x%x", 8)
			g.gtable[pair{g.gid, 1}] = g.tname + "_1"
		}
		if len(g.lptable2) != 0 {
			g.printTable(g.lptable2, fmt.Sprintf("%s_2[INAT_GROUP_TABLE_SIZE]", g.tname), "0x%x", 8)
			g.gtable[pair{g.gid, 2}] = g.tname + "_2"
		}
		if len(g.lptable3) != 0 {
			g.printTable(g.lptable3, fmt.Sprintf("%s_3[INAT_GROUP_TABLE_SIZE]", g.tname), "0x%x", 8)
			g.gtable[pair{g.gid, 3}] = g.tname + "_3"
		}
	} else {
		if len(g.table) != 0 {
			g.printTable(g.table, fmt.Sprintf("%s[INAT_OPCODE_TABLE_SIZE]", g.tname), "0x%02x", 256)
			g.etable[pair{g.eid, 0}] = g.tname
			if g.aid >= 0 {
				g.atable[pair{g.aid, 0}] = g.tname
			} else if g.xopid >= 0 {
				g.xoptable[g.xopid] = g.tname
			}
		}
		if len(g.lptable1) != 0 {
			g.printTable(g.lptable1, fmt.Sprintf("%s_1[INAT_OPCODE_TABLE_SIZE]", g.tname), "0x%02x", 256)
			g.etable[pair{g.eid, 1}] = g.tname + "_1"
			if g.aid >= 0 {
				g.atable[pair{g.aid, 1}] = g.tname + "_1"
			}
		}
		if len(g.lptable2) != 0 {
			g.printTable(g.lptable2, fmt.Sprintf("%s_2[INAT_OPCODE_TABLE_SIZE]", g.tname), "0x%02x", 256)
			g.etable[pair{g.eid, 2}] = g.tname + "_2"
			if g.aid >= 0 {
				g.atable[pair{g.aid, 2}] = g.tname + "_2"
			}
		}
		if len(g.lptable3) != 0 {
			g.printTable(g.lptable3, fmt.Sprintf("%s_3[INAT_OPCODE_TABLE_SIZE]", g.tname), "0x%02x", 256)
			g.etable[pair{g.eid, 3}] = g.tname + "_3"
			if g.aid >= 0 {
				g.atable[pair{g.aid, 3}] = g.tname + "_3"
			}
		}
	}
	fmt.Fprint(g.output, "\n")
	g.clearVars()
}

func (g *generator) printTable(table map[string]string, name, format string, n int) {
	fmt.Fprintf(g.output, "const insn_attr_t %s = {\n", name)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf(format, i)
		if value := table[id]; value != "" {
			fmt.Fprintf(g.output, "\t[%s] = %s,\n", id, value)
		}
	}
	fmt.Fprint(g.output, "};\n")
}

func (g *generator) opcodeLine(line string, fields []string) error {
	if g.line == 1 {
		return nil
	}
	idx := "0x" + strings.TrimSuffix(fields[0], ":")
	if _, ok := g.table[idx]; ok {
		return g.semanticError("Redefine " + idx + " in " + g.tname)
	}

	if len(fields) > 1 && fields[1] == "escape" {
		if len(fields) < 4 || fields[2] != "#" {
			return g.semanticError("No escaped name")
		}
		ref := strings.Join(fields[3:], "")
		if _, ok := g.escape[ref]; ok {
			return g.semanticError("Redefine escape (" + ref + ")")
		}
		g.escape[ref] = g.geid
		g.geid++
		g.table[idx] = fmt.Sprintf("INAT_MAKE_ESCAPE(%d)", g.escape[ref])
		return nil
	}

	variant := ""
	for i := 1; i < len(fields); {
		opcode := fields[i]
		i++
		ext := ""
		flags := ""
		if i < len(fields) && opndExpr.MatchString(fields[i]) {
			opnds := strings.Split(fields[i], ",")
			i++
			converted, err := g.convertOperands(opnds)
			if err != nil {
				return err
			}
			flags = converted
		}
		if i < len(fields) && extExpr.MatchString(fields[i]) {
			ext = fields[i]
			i++
		}
		if i < len(fields) && sepExpr.MatchString(fields[i]) {
			i++
		} else if i < len(fields) {
			return g.semanticError(fields[i] + " is not a separator")
		}

		if groupExpr.MatchString(opcode) {
			if _, ok := g.group[opcode]; !ok {
				g.group[opcode] = g.ggid
				g.ggid++
			}
			flags = addFlags(flags, fmt.Sprintf("INAT_MAKE_GROUP(%d)", g.group[opcode]))
		}
		if force64Expr.MatchString(ext) {
			flags = addFlags(flags, "INAT_FORCE64")
		}
		if g.supportsInvalid64 && invalid64Expr.MatchString(ext) && !only64Expr.MatchString(line) {
			flags = addFlags(flags, "INAT_INV64")
		}
		if noRex2Expr.MatchString(ext) {
			flags = addFlags(flags, "INAT_NO_REX2")
		}
		if rexExpr.MatchString(opcode) {
			flags = addFlags(flags, "INAT_MAKE_PREFIX(INAT_PFX_REX)")
		}
		if fpuExpr.MatchString(opcode) {
			flags = addFlags(flags, "INAT_MODRM")
		}
		switch {
		case evexOnlyExpr.MatchString(ext):
			flags = addFlags(flags, "INAT_VEXOK | INAT_EVEXONLY")
		case evexScalable.MatchString(ext):
			flags = addFlags(flags, "INAT_VEXOK | INAT_EVEXONLY | INAT_EVEX_SCALABLE")
		case vexOnlyExpr.MatchString(ext):
			flags = addFlags(flags, "INAT_VEXOK | INAT_VEXONLY")
		case vexOKExpr.MatchString(ext) || vexOKOpcodeExpr.MatchString(opcode):
			flags = addFlags(flags, "INAT_VEXOK")
		case xopOKExpr.MatchString(ext) || g.xopid >= 0:
			flags = addFlags(flags, "INAT_XOPOK")
		}
		if prefixExpr.MatchString(ext) {
			prefix, ok := prefixNum[opcode]
			if !ok {
				return g.semanticError("Unknown prefix: " + opcode)
			}
			flags = addFlags(flags, "INAT_MAKE_PREFIX("+prefix+")")
		}
		if flags == "" {
			continue
		}
		if lprefix1Expr.MatchString(ext) {
			g.lptable1[idx] = addFlags(g.lptable1[idx], flags)
			variant = "INAT_VARIANT"
		}
		if lprefix2Expr.MatchString(ext) {
			g.lptable2[idx] = addFlags(g.lptable2[idx], flags)
			variant = "INAT_VARIANT"
		}
		if lprefix3Expr.MatchString(ext) {
			g.lptable3[idx] = addFlags(g.lptable3[idx], flags)
			variant = "INAT_VARIANT"
		}
		if rex2Expr.MatchString(ext) {
			g.table[idx] = addFlags(g.table[idx], "INAT_REX2_VARIANT")
		}
		if !lprefixExpr.MatchString(ext) {
			g.table[idx] = addFlags(g.table[idx], flags)
		}
	}
	if variant != "" {
		g.table[idx] = addFlags(g.table[idx], variant)
	}
	return nil
}

func (g *generator) convertOperands(operands []string) (string, error) {
	imm := ""
	mod := ""
	for _, operand := range operands {
		if immExpr.MatchString(operand) {
			flag := immFlag[operand]
			if flag == "" {
				return "", g.semanticError("Unknown imm opnd: " + operand)
			}
			if imm != "" {
				if operand != "Ib" {
					return "", g.semanticError("Second IMM error")
				}
				imm = addFlags(imm, "INAT_SCNDIMM")
			} else {
				imm = flag
			}
		} else if modrmExpr.MatchString(operand) {
			mod = "INAT_MODRM"
		}
	}
	return addFlags(imm, mod), nil
}

func addFlags(old, new string) string {
	if old != "" && new != "" {
		return old + " | " + new
	}
	if old != "" {
		return old
	}
	return new
}

func (g *generator) finish() {
	fmt.Fprint(g.output, "#ifndef __BOOT_COMPRESSED\n\n")

	fmt.Fprint(g.output, "/* Escape opcode map array */\n")
	fmt.Fprint(g.output, "const insn_attr_t * const inat_escape_tables[INAT_ESC_MAX + 1][INAT_LSTPFX_MAX + 1] = {\n")
	for i := 0; i < g.geid; i++ {
		for j := 0; j < 4; j++ {
			if value := g.etable[pair{i, j}]; value != "" {
				fmt.Fprintf(g.output, "\t[%d][%d] = %s,\n", i, j, value)
			}
		}
	}
	fmt.Fprint(g.output, "};\n\n")

	fmt.Fprint(g.output, "/* Group opcode map array */\n")
	fmt.Fprint(g.output, "const insn_attr_t * const inat_group_tables[INAT_GRP_MAX + 1][INAT_LSTPFX_MAX + 1] = {\n")
	for i := 0; i < g.ggid; i++ {
		for j := 0; j < 4; j++ {
			if value := g.gtable[pair{i, j}]; value != "" {
				fmt.Fprintf(g.output, "\t[%d][%d] = %s,\n", i, j, value)
			}
		}
	}
	fmt.Fprint(g.output, "};\n\n")

	fmt.Fprint(g.output, "/* AVX opcode map array */\n")
	fmt.Fprint(g.output, "const insn_attr_t * const inat_avx_tables[X86_VEX_M_MAX + 1][INAT_LSTPFX_MAX + 1] = {\n")
	for i := 0; i < g.gaid; i++ {
		for j := 0; j < 4; j++ {
			if value := g.atable[pair{i, j}]; value != "" {
				fmt.Fprintf(g.output, "\t[%d][%d] = %s,\n", i, j, value)
			}
		}
	}
	fmt.Fprint(g.output, "};\n\n")

	if g.gxopid > 0 {
		fmt.Fprint(g.output, "/* XOP opcode map array */\n")
		fmt.Fprint(g.output, "const insn_attr_t * const inat_xop_tables[X86_XOP_M_MAX - X86_XOP_M_MIN + 1] = {\n")
		for i := 0; i < g.gxopid; i++ {
			if value := g.xoptable[i]; value != "" {
				fmt.Fprintf(g.output, "\t[%d] = %s,\n", i, value)
			}
		}
		fmt.Fprint(g.output, "};\n")
	}

	fmt.Fprint(g.output, "#else /* !__BOOT_COMPRESSED */\n\n")

	fmt.Fprint(g.output, "/* Escape opcode map array */\n")
	fmt.Fprint(g.output, "static const insn_attr_t *inat_escape_tables[INAT_ESC_MAX + 1][INAT_LSTPFX_MAX + 1];\n\n")
	fmt.Fprint(g.output, "/* Group opcode map array */\n")
	fmt.Fprint(g.output, "static const insn_attr_t *inat_group_tables[INAT_GRP_MAX + 1][INAT_LSTPFX_MAX + 1];\n\n")
	fmt.Fprint(g.output, "/* AVX opcode map array */\n")
	fmt.Fprint(g.output, "static const insn_attr_t *inat_avx_tables[X86_VEX_M_MAX + 1][INAT_LSTPFX_MAX + 1];\n\n")
	if g.gxopid > 0 {
		fmt.Fprint(g.output, "/* XOP opcode map array */\n")
		fmt.Fprint(g.output, "static const insn_attr_t *inat_xop_tables[X86_XOP_M_MAX - X86_XOP_M_MIN + 1];\n\n")
	}
	fmt.Fprint(g.output, "static void inat_init_tables(void)\n")
	fmt.Fprint(g.output, "{\n")

	fmt.Fprint(g.output, "\t/* Print Escape opcode map array */\n")
	for i := 0; i < g.geid; i++ {
		for j := 0; j < 4; j++ {
			if value := g.etable[pair{i, j}]; value != "" {
				fmt.Fprintf(g.output, "\tinat_escape_tables[%d][%d] = %s;\n", i, j, value)
			}
		}
	}
	fmt.Fprint(g.output, "\n")

	fmt.Fprint(g.output, "\t/* Print Group opcode map array */\n")
	for i := 0; i < g.ggid; i++ {
		for j := 0; j < 4; j++ {
			if value := g.gtable[pair{i, j}]; value != "" {
				fmt.Fprintf(g.output, "\tinat_group_tables[%d][%d] = %s;\n", i, j, value)
			}
		}
	}
	fmt.Fprint(g.output, "\n")

	fmt.Fprint(g.output, "\t/* Print AVX opcode map array */\n")
	for i := 0; i < g.gaid; i++ {
		for j := 0; j < 4; j++ {
			if value := g.atable[pair{i, j}]; value != "" {
				fmt.Fprintf(g.output, "\tinat_avx_tables[%d][%d] = %s;\n", i, j, value)
			}
		}
	}
	fmt.Fprint(g.output, "\n")

	if g.gxopid > 0 {
		fmt.Fprint(g.output, "\t/* Print XOP opcode map array */\n")
		for i := 0; i < g.gxopid; i++ {
			if value := g.xoptable[i]; value != "" {
				fmt.Fprintf(g.output, "\tinat_xop_tables[%d] = %s;\n", i, value)
			}
		}
	}
	fmt.Fprint(g.output, "}\n")
	fmt.Fprint(g.output, "#endif\n")
}
