// closing_xlsx.go renders the consolidated cierre report as a polished,
// multi-sheet Excel workbook using github.com/xuri/excelize/v2 (pure Go, so the
// single-binary deployment is preserved). The workbook is styled for a director
// to read at a glance: bold titles, section headers, COP currency formatting,
// frozen header rows and sensible column widths.
package reports

import (
	"fmt"
	"time"

	"github.com/xuri/excelize/v2"
)

// Palette used across the workbook.
const (
	colorTitle   = "1F2937" // slate-800
	colorHeader  = "2563EB" // blue-600
	colorSection = "DBEAFE" // blue-100
	colorBandRow = "F3F4F6" // gray-100
	moneyFmt     = `"$"#,##0`
)

// xlsxStyles bundles the reusable cell styles for one workbook.
type xlsxStyles struct {
	title   int
	subtle  int
	section int
	header  int
	money   int
	moneyB  int // banded money cell
	text    int
	textB   int // banded text cell
	num     int
	numB    int
	kpiKey  int
	kpiVal  int
}

func newStyles(f *excelize.File) (xlsxStyles, error) {
	var s xlsxStyles
	var err error
	mk := func(st *excelize.Style) int {
		if err != nil {
			return 0
		}
		var id int
		id, err = f.NewStyle(st)
		return id
	}

	s.title = mk(&excelize.Style{
		Font: &excelize.Font{Bold: true, Size: 18, Color: colorTitle},
	})
	s.subtle = mk(&excelize.Style{
		Font: &excelize.Font{Italic: true, Size: 10, Color: "6B7280"},
	})
	s.section = mk(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Size: 12, Color: colorTitle},
		Fill:      excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{colorSection}},
		Alignment: &excelize.Alignment{Vertical: "center"},
	})
	s.header = mk(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Color: "FFFFFF"},
		Fill:      excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{colorHeader}},
		Alignment: &excelize.Alignment{Horizontal: "center", Vertical: "center"},
		Border:    thinBorder(),
	})
	s.money = mk(&excelize.Style{CustomNumFmt: ptr(moneyFmt), Border: thinBorder()})
	s.moneyB = mk(&excelize.Style{
		CustomNumFmt: ptr(moneyFmt), Border: thinBorder(),
		Fill: excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{colorBandRow}},
	})
	s.text = mk(&excelize.Style{Border: thinBorder()})
	s.textB = mk(&excelize.Style{
		Border: thinBorder(),
		Fill:   excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{colorBandRow}},
	})
	s.num = mk(&excelize.Style{
		Border: thinBorder(), Alignment: &excelize.Alignment{Horizontal: "right"},
	})
	s.numB = mk(&excelize.Style{
		Border: thinBorder(), Alignment: &excelize.Alignment{Horizontal: "right"},
		Fill: excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{colorBandRow}},
	})
	s.kpiKey = mk(&excelize.Style{
		Font: &excelize.Font{Bold: true, Color: colorTitle}, Border: thinBorder(),
	})
	s.kpiVal = mk(&excelize.Style{
		Border: thinBorder(), Alignment: &excelize.Alignment{Horizontal: "right"},
	})
	return s, err
}

func ptr(s string) *string { return &s }

func thinBorder() []excelize.Border {
	return []excelize.Border{
		{Type: "left", Color: "D1D5DB", Style: 1},
		{Type: "right", Color: "D1D5DB", Style: 1},
		{Type: "top", Color: "D1D5DB", Style: 1},
		{Type: "bottom", Color: "D1D5DB", Style: 1},
	}
}

// cell returns the A1 reference for 1-based col,row (ignoring conversion errors,
// which cannot happen for the small, valid coordinates used here).
func cell(col, row int) string {
	c, _ := excelize.CoordinatesToCellName(col, row)
	return c
}

// buildClosingWorkbook assembles the full multi-sheet workbook.
func buildClosingWorkbook(rep ClosingResponse, sales []SaleRow, att []closingAttendee, inv []closingInventoryRow) (*excelize.File, error) {
	f := excelize.NewFile()
	st, err := newStyles(f)
	if err != nil {
		return nil, err
	}

	// The default "Sheet1" becomes "Resumen"; the rest are appended in order.
	if err := f.SetSheetName("Sheet1", "Resumen"); err != nil {
		return nil, err
	}
	for _, name := range []string{"Ventas", "Productos", "Por vendedor", "Asistentes", "Inventario"} {
		if _, err := f.NewSheet(name); err != nil {
			return nil, err
		}
	}

	if err := writeResumenSheet(f, st, rep); err != nil {
		return nil, err
	}
	if err := writeVentasSheet(f, st, rep, sales); err != nil {
		return nil, err
	}
	if err := writeProductosSheet(f, st, rep); err != nil {
		return nil, err
	}
	if err := writeVendedorSheet(f, st, rep); err != nil {
		return nil, err
	}
	if err := writeAsistentesSheet(f, st, rep, att); err != nil {
		return nil, err
	}
	if err := writeInventarioSheet(f, st, rep, inv); err != nil {
		return nil, err
	}

	if idx, err := f.GetSheetIndex("Resumen"); err == nil {
		f.SetActiveSheet(idx)
	}
	return f, nil
}

// titleRows writes the event title + a generated-at subtitle on rows 1-2 and
// returns the next free row.
func titleRows(f *excelize.File, st xlsxStyles, sheet, heading string) int {
	_ = f.SetCellValue(sheet, "A1", heading)
	_ = f.SetCellStyle(sheet, "A1", "A1", st.title)
	_ = f.SetCellValue(sheet, "A2", "Generado: "+time.Now().Format("2006-01-02 15:04:05"))
	_ = f.SetCellStyle(sheet, "A2", "A2", st.subtle)
	_ = f.SetRowHeight(sheet, 1, 26)
	return 4 // leave row 3 blank
}

// freezeAt freezes everything above the given 1-based row.
func freezeAt(f *excelize.File, sheet string, row int) {
	_ = f.SetPanes(sheet, &excelize.Panes{
		Freeze:      true,
		YSplit:      row - 1,
		TopLeftCell: cell(1, row),
		ActivePane:  "bottomLeft",
	})
}

// writeHeader writes a styled header row and returns the next row.
func writeHeader(f *excelize.File, st xlsxStyles, sheet string, row int, cols []string) int {
	for i, c := range cols {
		ref := cell(i+1, row)
		_ = f.SetCellValue(sheet, ref, c)
		_ = f.SetCellStyle(sheet, ref, ref, st.header)
	}
	_ = f.SetRowHeight(sheet, row, 18)
	return row + 1
}

// ---------------------------------------------------------------------------
// Resumen (KPIs)
// ---------------------------------------------------------------------------

func writeResumenSheet(f *excelize.File, st xlsxStyles, rep ClosingResponse) error {
	const sheet = "Resumen"
	_ = f.SetColWidth(sheet, "A", "A", 38)
	_ = f.SetColWidth(sheet, "B", "B", 22)

	_ = f.SetCellValue(sheet, "A1", "Cierre de evento — "+rep.Event.Name)
	_ = f.SetCellStyle(sheet, "A1", "A1", st.title)
	_ = f.SetRowHeight(sheet, 1, 28)
	_ = f.SetCellValue(sheet, "A2", fmt.Sprintf("Estado: %s · Generado: %s",
		rep.Event.Status, time.Now().Format("2006-01-02 15:04:05")))
	_ = f.SetCellStyle(sheet, "A2", "A2", st.subtle)

	row := 4
	section := func(label string) {
		ref := cell(1, row)
		_ = f.SetCellValue(sheet, ref, label)
		_ = f.MergeCell(sheet, cell(1, row), cell(2, row))
		_ = f.SetCellStyle(sheet, cell(1, row), cell(2, row), st.section)
		_ = f.SetRowHeight(sheet, row, 20)
		row++
	}
	kpiMoney := func(label string, v int64) {
		_ = f.SetCellValue(sheet, cell(1, row), label)
		_ = f.SetCellStyle(sheet, cell(1, row), cell(1, row), st.kpiKey)
		_ = f.SetCellValue(sheet, cell(2, row), v)
		_ = f.SetCellStyle(sheet, cell(2, row), cell(2, row), st.money)
		row++
	}
	kpiText := func(label, v string) {
		_ = f.SetCellValue(sheet, cell(1, row), label)
		_ = f.SetCellStyle(sheet, cell(1, row), cell(1, row), st.kpiKey)
		_ = f.SetCellValue(sheet, cell(2, row), v)
		_ = f.SetCellStyle(sheet, cell(2, row), cell(2, row), st.kpiVal)
		row++
	}

	section("Resultados financieros")
	kpiMoney("Ingresos", rep.Totals.Revenue)
	kpiMoney("Costo de lo vendido", rep.Totals.Cost)
	kpiMoney("Ganancia", rep.Totals.Profit)
	kpiText("Margen", fmt.Sprintf("%.1f%%", rep.Totals.Margin*100))
	kpiText("Pedidos", fmt.Sprintf("%d", rep.Totals.OrderCount))
	kpiMoney("Excedente en efectivo", rep.Totals.OverageCash)
	kpiMoney("Saldo usado en ventas", rep.Totals.BalanceFromSales)
	row++

	section("Clientes")
	kpiText("Clientes registrados", fmt.Sprintf("%d", rep.Attendees.Registered))
	kpiText("Clientes activos", fmt.Sprintf("%d", rep.Attendees.Active))
	kpiMoney("Consumo promedio por cliente", int64(rep.Attendees.AvgConsumption+0.5))
	kpiMoney("Dinero pendiente (saldo sin gastar)", rep.Attendees.PendingBalance)
	row++

	section("Tiempos de atención")
	kpiText("Tiempo promedio por pedido", fmt.Sprintf("%.1f s", rep.Timing.AvgSecondsPerOrder))
	if rep.Timing.FastestOperator != nil {
		kpiText("Vendedor más rápido", rep.Timing.FastestOperator.Username)
		kpiText("  · tiempo promedio", fmt.Sprintf("%.1f s", rep.Timing.FastestOperator.AvgSeconds))
		kpiText("  · pedidos atendidos", fmt.Sprintf("%d", rep.Timing.FastestOperator.OrderCount))
	} else {
		kpiText("Vendedor más rápido", "n/a (mínimo 3 pedidos)")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Ventas (order list)
// ---------------------------------------------------------------------------

func writeVentasSheet(f *excelize.File, st xlsxStyles, rep ClosingResponse, sales []SaleRow) error {
	const sheet = "Ventas"
	_ = f.SetColWidth(sheet, "A", "A", 28)
	_ = f.SetColWidth(sheet, "B", "B", 20)
	_ = f.SetColWidth(sheet, "C", "C", 18)
	_ = f.SetColWidth(sheet, "D", "F", 16)
	_ = f.SetColWidth(sheet, "G", "G", 12)

	row := titleRows(f, st, sheet, "Ventas — "+rep.Event.Name)
	row = writeHeader(f, st, sheet, row, []string{
		"Pedido", "Fecha", "Vendedor", "Total", "Saldo", "Efectivo", "Ítems",
	})
	headerRow := row
	for i, s := range sales {
		txt, num, mon := bandStyles(st, i)
		put(f, sheet, cell(1, row), s.ID, txt)
		put(f, sheet, cell(2, row), time.UnixMilli(s.CreatedAt).Format("2006-01-02 15:04"), txt)
		put(f, sheet, cell(3, row), s.OperatorName, txt)
		put(f, sheet, cell(4, row), s.Total, mon)
		put(f, sheet, cell(5, row), s.PaidFromBalance, mon)
		put(f, sheet, cell(6, row), s.PaidCash, mon)
		put(f, sheet, cell(7, row), s.ItemCount, num)
		row++
	}
	freezeAt(f, sheet, headerRow)
	return nil
}

// ---------------------------------------------------------------------------
// Productos (top products)
// ---------------------------------------------------------------------------

func writeProductosSheet(f *excelize.File, st xlsxStyles, rep ClosingResponse) error {
	const sheet = "Productos"
	_ = f.SetColWidth(sheet, "A", "A", 34)
	_ = f.SetColWidth(sheet, "B", "B", 12)
	_ = f.SetColWidth(sheet, "C", "E", 16)

	row := titleRows(f, st, sheet, "Productos — "+rep.Event.Name)
	row = writeHeader(f, st, sheet, row, []string{
		"Producto", "Unidades", "Ingreso", "Costo", "Ganancia",
	})
	headerRow := row
	for i, p := range rep.TopProducts {
		txt, num, mon := bandStyles(st, i)
		put(f, sheet, cell(1, row), p.Name, txt)
		put(f, sheet, cell(2, row), p.QtySold, num)
		put(f, sheet, cell(3, row), p.Revenue, mon)
		put(f, sheet, cell(4, row), p.Cost, mon)
		put(f, sheet, cell(5, row), p.Profit, mon)
		row++
	}
	freezeAt(f, sheet, headerRow)
	return nil
}

// ---------------------------------------------------------------------------
// Por vendedor (operators + timing)
// ---------------------------------------------------------------------------

func writeVendedorSheet(f *excelize.File, st xlsxStyles, rep ClosingResponse) error {
	const sheet = "Por vendedor"
	_ = f.SetColWidth(sheet, "A", "A", 26)
	_ = f.SetColWidth(sheet, "B", "B", 18)
	_ = f.SetColWidth(sheet, "C", "C", 12)
	_ = f.SetColWidth(sheet, "D", "D", 18)

	row := titleRows(f, st, sheet, "Por vendedor — "+rep.Event.Name)
	row = writeHeader(f, st, sheet, row, []string{
		"Vendedor", "Total", "Pedidos", "Tiempo prom. (s)",
	})
	headerRow := row
	for i, o := range rep.Operators {
		txt, num, mon := bandStyles(st, i)
		put(f, sheet, cell(1, row), o.Username, txt)
		put(f, sheet, cell(2, row), o.Total, mon)
		put(f, sheet, cell(3, row), o.Count, num)
		put(f, sheet, cell(4, row), roundOne(o.AvgSeconds), num)
		row++
	}
	freezeAt(f, sheet, headerRow)
	return nil
}

// ---------------------------------------------------------------------------
// Asistentes
// ---------------------------------------------------------------------------

func writeAsistentesSheet(f *excelize.File, st xlsxStyles, rep ClosingResponse, att []closingAttendee) error {
	const sheet = "Asistentes"
	_ = f.SetColWidth(sheet, "A", "A", 28)
	_ = f.SetColWidth(sheet, "B", "C", 18)
	_ = f.SetColWidth(sheet, "D", "D", 16)
	_ = f.SetColWidth(sheet, "E", "E", 12)

	row := titleRows(f, st, sheet, "Asistentes — "+rep.Event.Name)
	row = writeHeader(f, st, sheet, row, []string{
		"Nombre", "Documento", "Teléfono", "Saldo", "Estado",
	})
	headerRow := row
	for i, a := range att {
		txt, _, mon := bandStyles(st, i)
		put(f, sheet, cell(1, row), a.Name, txt)
		put(f, sheet, cell(2, row), a.DocID, txt)
		put(f, sheet, cell(3, row), a.Phone, txt)
		put(f, sheet, cell(4, row), a.Balance, mon)
		put(f, sheet, cell(5, row), a.Status, txt)
		row++
	}
	freezeAt(f, sheet, headerRow)
	return nil
}

// ---------------------------------------------------------------------------
// Inventario (remaining stock + out-of-stock summary)
// ---------------------------------------------------------------------------

func writeInventarioSheet(f *excelize.File, st xlsxStyles, rep ClosingResponse, inv []closingInventoryRow) error {
	const sheet = "Inventario"
	_ = f.SetColWidth(sheet, "A", "A", 34)
	_ = f.SetColWidth(sheet, "B", "B", 16)
	_ = f.SetColWidth(sheet, "C", "C", 14)
	_ = f.SetColWidth(sheet, "D", "D", 12)

	row := titleRows(f, st, sheet, "Inventario — "+rep.Event.Name)
	row = writeHeader(f, st, sheet, row, []string{
		"Producto", "Controla stock", "Stock restante", "Activo",
	})
	headerRow := row
	for i, p := range inv {
		txt, num, _ := bandStyles(st, i)
		put(f, sheet, cell(1, row), p.Name, txt)
		put(f, sheet, cell(2, row), yesNo(p.TrackStock), txt)
		put(f, sheet, cell(3, row), p.StockQty, num)
		put(f, sheet, cell(4, row), yesNo(p.Active), txt)
		row++
	}
	freezeAt(f, sheet, headerRow)

	// Productos agotados summary below the table.
	row += 2
	_ = f.SetCellValue(sheet, cell(1, row), "Productos agotados")
	_ = f.MergeCell(sheet, cell(1, row), cell(2, row))
	_ = f.SetCellStyle(sheet, cell(1, row), cell(2, row), st.section)
	row++
	if len(rep.Inventory.OutOfStock) == 0 {
		put(f, sheet, cell(1, row), "Ninguno", st.text)
	} else {
		for i, it := range rep.Inventory.OutOfStock {
			txt, _, _ := bandStyles(st, i)
			put(f, sheet, cell(1, row), it.Name, txt)
			row++
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

// bandStyles returns the (text, number, money) styles for data row index i,
// alternating shading for readability.
func bandStyles(st xlsxStyles, i int) (text, num, money int) {
	if i%2 == 1 {
		return st.textB, st.numB, st.moneyB
	}
	return st.text, st.num, st.money
}

func put(f *excelize.File, sheet, ref string, v any, style int) {
	_ = f.SetCellValue(sheet, ref, v)
	_ = f.SetCellStyle(sheet, ref, ref, style)
}

func yesNo(v int64) string {
	if v == 1 {
		return "Sí"
	}
	return "No"
}

func roundOne(v float64) float64 {
	return float64(int64(v*10+0.5)) / 10
}
