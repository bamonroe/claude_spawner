package com.bam.spawner.ui

import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.LocalContentColor
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.remember
import androidx.compose.ui.Modifier
import androidx.compose.ui.layout.Layout
import androidx.compose.ui.text.AnnotatedString
import androidx.compose.ui.text.LinkAnnotation
import androidx.compose.ui.text.SpanStyle
import androidx.compose.ui.text.TextLinkStyles
import androidx.compose.ui.text.buildAnnotatedString
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontStyle
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextDecoration
import androidx.compose.ui.text.withLink
import androidx.compose.ui.text.withStyle
import androidx.compose.ui.unit.Constraints
import androidx.compose.ui.unit.dp

/**
 * Renders Claude's markdown natively in Compose — headings, bold/italic, inline
 * code, fenced code blocks, bullet/numbered lists, GFM tables, and links.
 * Deliberately small (no external dependency); covers the common cases Claude
 * produces.
 */
@Composable
fun MarkdownText(text: String, modifier: Modifier = Modifier) {
    val blocks = remember(text) { parseBlocks(text) }
    Column(modifier, verticalArrangement = Arrangement.spacedBy(4.dp)) {
        for (b in blocks) {
            when (b) {
                is MdTable -> MdTableView(b)
                is MdHeader -> Text(
                    inline(b.text),
                    style = when (b.level) {
                        1 -> MaterialTheme.typography.titleLarge
                        2 -> MaterialTheme.typography.titleMedium
                        else -> MaterialTheme.typography.titleSmall
                    },
                    fontWeight = FontWeight.Bold,
                )
                is MdCode -> Surface(
                    color = LocalContentColor.current.copy(alpha = 0.10f),
                    shape = RoundedCornerShape(6.dp),
                    modifier = Modifier.fillMaxWidth(),
                ) {
                    Text(
                        b.text, Modifier.padding(8.dp),
                        fontFamily = FontFamily.Monospace,
                        style = MaterialTheme.typography.bodySmall,
                    )
                }
                is MdBullet -> Row {
                    Text("•  ")
                    Text(inline(b.text), Modifier.weight(1f), style = MaterialTheme.typography.bodyMedium)
                }
                is MdNumbered -> Row {
                    Text("${b.marker}  ")
                    Text(inline(b.text), Modifier.weight(1f), style = MaterialTheme.typography.bodyMedium)
                }
                is MdParagraph -> Text(inline(b.text), style = MaterialTheme.typography.bodyMedium)
            }
        }
    }
}

// Internal (not private) so the block parser is unit-testable — see MarkdownTableTest.
internal sealed interface MdBlock
internal data class MdParagraph(val text: String) : MdBlock
internal data class MdHeader(val level: Int, val text: String) : MdBlock
internal data class MdCode(val text: String) : MdBlock
internal data class MdBullet(val text: String) : MdBlock
internal data class MdNumbered(val marker: String, val text: String) : MdBlock

/**
 * A GFM pipe table. `aligns` comes from the separator row's colons (`:--`, `--:`,
 * `:--:`) and is one entry per column; `rows` are the body rows, which may be
 * ragged (missing cells render empty).
 */
internal data class MdTable(
    val header: List<String>,
    val aligns: List<TextAlign>,
    val rows: List<List<String>>,
) : MdBlock

private val bulletRe = Regex("^[-*+]\\s+")
private val numRe = Regex("^(\\d+)\\.\\s+")
private val sepCellRe = Regex("^:?-+:?$")

/**
 * Splits one table row into trimmed cells on unescaped pipes, dropping the
 * optional leading/trailing pipe. `\|` is a literal pipe inside a cell.
 */
internal fun splitTableRow(line: String): List<String> {
    var t = line.trim()
    if (t.startsWith("|")) t = t.substring(1)
    if (t.endsWith("|") && !t.endsWith("\\|")) t = t.dropLast(1)
    val cells = mutableListOf<String>()
    val sb = StringBuilder()
    var i = 0
    while (i < t.length) {
        val c = t[i]
        when {
            c == '\\' && i + 1 < t.length && t[i + 1] == '|' -> { sb.append('|'); i += 2 }
            c == '|' -> { cells.add(sb.trim().toString()); sb.clear(); i++ }
            else -> { sb.append(c); i++ }
        }
    }
    cells.add(sb.trim().toString())
    return cells
}

/** The cell alignments of a separator row, or null if the line isn't one. */
private fun separatorAligns(line: String): List<TextAlign>? {
    val cells = splitTableRow(line)
    if (cells.isEmpty() || cells.any { !sepCellRe.matches(it) }) return null
    return cells.map {
        when {
            it.startsWith(":") && it.endsWith(":") -> TextAlign.Center
            it.endsWith(":") -> TextAlign.End
            else -> TextAlign.Start
        }
    }
}

internal fun parseBlocks(md: String): List<MdBlock> {
    val blocks = mutableListOf<MdBlock>()
    val lines = md.split("\n")
    val para = StringBuilder()
    fun flush() {
        if (para.isNotBlank()) blocks.add(MdParagraph(para.trim().toString()))
        para.clear()
    }
    var i = 0
    while (i < lines.size) {
        val line = lines[i]
        val t = line.trimStart()
        when {
            t.startsWith("```") -> {
                flush()
                val sb = StringBuilder()
                i++
                while (i < lines.size && !lines[i].trimStart().startsWith("```")) {
                    sb.append(lines[i]).append('\n'); i++
                }
                i++ // skip closing fence
                blocks.add(MdCode(sb.toString().trimEnd('\n')))
                continue
            }
            // A GFM table: a pipe-bearing header line whose successor is a separator
            // row (`---|:--:`) with the SAME cell count. Requiring the counts to match
            // keeps a paragraph line containing a pipe followed by a `---` horizontal
            // rule from being mistaken for a one-column table. Checked before the
            // bullet/number cases so a leading-pipe-less table still wins.
            t.contains('|') && i + 1 < lines.size &&
                separatorAligns(lines[i + 1])?.size == splitTableRow(t).size -> {
                flush()
                val header = splitTableRow(t)
                val aligns = separatorAligns(lines[i + 1])!!
                i += 2
                val rows = mutableListOf<List<String>>()
                while (i < lines.size && lines[i].contains('|')) {
                    rows.add(splitTableRow(lines[i])); i++
                }
                blocks.add(MdTable(header, aligns, rows))
                continue
            }
            t.startsWith("#") -> {
                flush()
                val hashes = t.takeWhile { it == '#' }.length
                blocks.add(MdHeader(hashes.coerceIn(1, 6), t.drop(hashes).trim()))
            }
            bulletRe.containsMatchIn(t) -> {
                flush(); blocks.add(MdBullet(t.replaceFirst(bulletRe, "")))
            }
            numRe.containsMatchIn(t) -> {
                flush()
                val m = numRe.find(t)!!
                blocks.add(MdNumbered(m.groupValues[1] + ".", t.substring(m.value.length)))
            }
            t.isBlank() -> flush()
            else -> {
                if (para.isNotEmpty()) para.append(' ')
                para.append(t)
            }
        }
        i++
    }
    flush()
    return blocks
}

/**
 * Shrinks natural column widths to fit `avail`, proportionally but never below a
 * floor of half an even share — so one very wide column can't squeeze its
 * neighbours down to a single character. Returns the natural widths unchanged
 * when the table already fits.
 */
internal fun fitColumns(want: IntArray, avail: Int): IntArray {
    val total = want.sum()
    if (want.isEmpty() || avail <= 0 || total <= avail) return want.copyOf()
    val floor = (avail / want.size / 2).coerceAtLeast(1)
    val out = IntArray(want.size)
    val flexible = BooleanArray(want.size)
    var fixed = 0
    var flexWant = 0
    for (c in want.indices) {
        // Columns whose proportional share would fall under the floor are pinned at
        // it (or at their natural width, whichever is smaller) and take no further
        // part; the rest split what's left in proportion to what they wanted.
        if (want[c].toLong() * avail / total < floor) {
            out[c] = minOf(floor, want[c]); fixed += out[c]
        } else {
            flexible[c] = true; flexWant += want[c]
        }
    }
    val left = (avail - fixed).coerceAtLeast(0)
    for (c in want.indices) if (flexible[c]) {
        out[c] = (if (flexWant > 0) (want[c].toLong() * left / flexWant).toInt() else 0).coerceAtLeast(1)
    }
    return out
}

/**
 * Draws an MdTable as a real grid. Column widths come from each column's widest
 * cell (see fitColumns), so a table narrower than the bubble stays narrow and a
 * wider one wraps its long cells instead of clipping them or running off-screen.
 * The header tint, the grid lines and every cell are children of a single Layout,
 * so one measure pass positions the lot — the tint and the lines are sized from
 * the measured row heights, which is what a Column-of-Rows can't do.
 */
@Composable
private fun MdTableView(table: MdTable, modifier: Modifier = Modifier) {
    val rows = remember(table) { listOf(table.header) + table.rows }
    val cols = remember(table) { rows.maxOf { it.size }.coerceAtLeast(1) }
    val lineColor = LocalContentColor.current.copy(alpha = 0.30f)
    val headerBg = LocalContentColor.current.copy(alpha = 0.08f)
    Layout(
        modifier = modifier.border(1.dp, lineColor, RoundedCornerShape(4.dp)),
        content = {
            Box(Modifier.background(headerBg))          // child 0: header tint, placed behind
            for ((r, row) in rows.withIndex()) for (c in 0 until cols) {
                Box(Modifier.padding(horizontal = 6.dp, vertical = 4.dp)) {
                    Text(
                        inline(row.getOrElse(c) { "" }),
                        Modifier.fillMaxWidth(),        // so textAlign has room to act
                        style = MaterialTheme.typography.bodySmall,
                        fontWeight = if (r == 0) FontWeight.Bold else FontWeight.Normal,
                        textAlign = table.aligns.getOrElse(c) { TextAlign.Start },
                    )
                }
            }
            repeat(cols - 1 + rows.size - 1) { Box(Modifier.background(lineColor)) }
        },
    ) { measurables, constraints ->
        val lineW = 1.dp.roundToPx().coerceAtLeast(1)
        val cells = measurables.subList(1, 1 + rows.size * cols)
        val gridLines = measurables.subList(1 + rows.size * cols, measurables.size)
        val want = IntArray(cols)
        cells.forEachIndexed { i, m ->
            val c = i % cols
            want[c] = maxOf(want[c], m.maxIntrinsicWidth(Constraints.Infinity))
        }
        val widths = if (constraints.hasBoundedWidth) {
            fitColumns(want, constraints.maxWidth - (cols - 1) * lineW)
        } else {
            want
        }
        val placeables = cells.mapIndexed { i, m ->
            val w = widths[i % cols]
            m.measure(Constraints(minWidth = w, maxWidth = w))
        }
        val rowH = IntArray(rows.size)
        placeables.forEachIndexed { i, p -> rowH[i / cols] = maxOf(rowH[i / cols], p.height) }
        val xs = IntArray(cols)
        val ys = IntArray(rows.size)
        for (c in 1 until cols) xs[c] = xs[c - 1] + widths[c - 1] + lineW
        for (r in 1 until rows.size) ys[r] = ys[r - 1] + rowH[r - 1] + lineW
        val totalW = xs[cols - 1] + widths[cols - 1]
        val totalH = ys[rows.size - 1] + rowH[rows.size - 1]
        val tint = measurables[0].measure(Constraints.fixed(totalW, rowH[0]))
        var next = 0
        val vLines = (1 until cols).map { gridLines[next++].measure(Constraints.fixed(lineW, totalH)) }
        val hLines = (1 until rows.size).map { gridLines[next++].measure(Constraints.fixed(totalW, lineW)) }
        layout(totalW.coerceIn(constraints.minWidth, maxOf(constraints.minWidth, constraints.maxWidth)), totalH) {
            tint.placeRelative(0, 0)
            placeables.forEachIndexed { i, p -> p.placeRelative(xs[i % cols], ys[i / cols]) }
            vLines.forEachIndexed { k, p -> p.placeRelative(xs[k + 1] - lineW, 0) }
            hLines.forEachIndexed { k, p -> p.placeRelative(0, ys[k + 1] - lineW) }
        }
    }
}

/** Parses inline markdown (**bold**, *italic*, `code`, [text](url)) to styled text. */
@Composable
private fun inline(text: String): AnnotatedString {
    val codeBg = LocalContentColor.current.copy(alpha = 0.12f)
    val linkColor = MaterialTheme.colorScheme.primary
    return remember(text, codeBg, linkColor) {
        buildAnnotatedString {
            var i = 0
            while (i < text.length) {
                when {
                    text.startsWith("**", i) || text.startsWith("__", i) -> {
                        val marker = text.substring(i, i + 2)
                        val end = text.indexOf(marker, i + 2)
                        if (end >= 0) {
                            withStyle(SpanStyle(fontWeight = FontWeight.Bold)) { append(text.substring(i + 2, end)) }
                            i = end + 2
                        } else { append(text[i]); i++ }
                    }
                    text[i] == '*' || text[i] == '_' -> {
                        val end = text.indexOf(text[i], i + 1)
                        if (end >= 0) {
                            withStyle(SpanStyle(fontStyle = FontStyle.Italic)) { append(text.substring(i + 1, end)) }
                            i = end + 1
                        } else { append(text[i]); i++ }
                    }
                    text[i] == '`' -> {
                        val end = text.indexOf('`', i + 1)
                        if (end >= 0) {
                            withStyle(SpanStyle(fontFamily = FontFamily.Monospace, background = codeBg)) {
                                append(text.substring(i + 1, end))
                            }
                            i = end + 1
                        } else { append(text[i]); i++ }
                    }
                    text[i] == '[' -> {
                        val close = text.indexOf(']', i)
                        if (close >= 0 && close + 1 < text.length && text[close + 1] == '(') {
                            val paren = text.indexOf(')', close + 2)
                            if (paren >= 0) {
                                val url = text.substring(close + 2, paren)
                                val linkStyle = SpanStyle(color = linkColor, textDecoration = TextDecoration.Underline)
                                withLink(LinkAnnotation.Url(url, styles = TextLinkStyles(style = linkStyle))) {
                                    append(text.substring(i + 1, close))
                                }
                                i = paren + 1
                            } else { append(text[i]); i++ }
                        } else { append(text[i]); i++ }
                    }
                    else -> { append(text[i]); i++ }
                }
            }
        }
    }
}
