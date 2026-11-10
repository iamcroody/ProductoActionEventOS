// Compact QR Code generator (byte mode, error-correction level M).
// Ported from the public-domain algorithm by Kazuhiko Arase (qrcode-generator),
// trimmed to byte mode only. Returns a boolean module matrix.

const PAD0 = 0xec;
const PAD1 = 0x11;

// --- Galois field math -----------------------------------------------------
const EXP: number[] = new Array(256);
const LOG: number[] = new Array(256);
(() => {
  for (let i = 0; i < 8; i++) EXP[i] = 1 << i;
  for (let i = 8; i < 256; i++) EXP[i] = EXP[i - 4] ^ EXP[i - 5] ^ EXP[i - 6] ^ EXP[i - 8];
  for (let i = 0; i < 255; i++) LOG[EXP[i]] = i;
})();
const gexp = (n: number) => { while (n < 0) n += 255; while (n >= 255) n -= 255; return EXP[n]; };
const glog = (n: number) => LOG[n];

class Poly {
  num: number[];
  constructor(num: number[], shift = 0) {
    let offset = 0;
    while (offset < num.length && num[offset] === 0) offset++;
    this.num = new Array(num.length - offset + shift);
    for (let i = 0; i < num.length - offset; i++) this.num[i] = num[i + offset];
  }
  get(i: number) { return this.num[i]; }
  len() { return this.num.length; }
  multiply(e: Poly): Poly {
    const num = new Array(this.len() + e.len() - 1).fill(0);
    for (let i = 0; i < this.len(); i++)
      for (let j = 0; j < e.len(); j++)
        num[i + j] ^= gexp(glog(this.get(i)) + glog(e.get(j)));
    return new Poly(num);
  }
  mod(e: Poly): Poly {
    if (this.len() - e.len() < 0) return this;
    const ratio = glog(this.get(0)) - glog(e.get(0));
    const num = this.num.slice();
    for (let i = 0; i < e.len(); i++) num[i] ^= gexp(glog(e.get(i)) + ratio);
    return new Poly(num).mod(e);
  }
}

function rsPoly(count: number): Poly {
  let p = new Poly([1]);
  for (let i = 0; i < count; i++) p = p.multiply(new Poly([1, gexp(i)]));
  return p;
}

// --- BCH for format/version info -------------------------------------------
const G15 = (1 << 10) | (1 << 8) | (1 << 5) | (1 << 4) | (1 << 2) | (1 << 1) | 1;
const G15_MASK = (1 << 14) | (1 << 12) | (1 << 10) | (1 << 4) | (1 << 1);
function bchDigit(data: number) { let d = 0; while (data !== 0) { d++; data >>>= 1; } return d; }
function bch15(data: number) {
  let d = data << 10;
  while (bchDigit(d) - bchDigit(G15) >= 0) d ^= G15 << (bchDigit(d) - bchDigit(G15));
  return ((data << 10) | d) ^ G15_MASK;
}

// RS block layout for EC level M, versions 1..10 (enough for QR tokens).
// Each entry: [totalCount, dataCount] possibly repeated.
const RS_M: Record<number, number[][]> = {
  1: [[26, 16]],
  2: [[44, 28]],
  3: [[70, 44]],
  4: [[100, 64]],
  5: [[134, 86]],
  6: [[86, 58], [86, 58]],
  7: [[98, 64], [98, 64]],
  8: [[121, 84], [121, 84]],
  9: [[146, 102], [146, 102]],
  10: [[86, 58], [86, 58], [86, 58], [86, 58]],
};
// data capacity (bytes) for byte mode, EC level M, versions 1..10
const CAP_M = [0, 14, 26, 42, 62, 84, 106, 122, 152, 180, 213];

function chooseVersion(len: number): number {
  for (let v = 1; v <= 10; v++) if (len + 0 <= CAP_M[v]) return v;
  throw new Error('Texto demasiado largo para QR');
}

interface RsBlock { total: number; data: number[]; ec: number[]; }

export function makeQr(text: string): boolean[][] {
  const bytes: number[] = [];
  for (let i = 0; i < text.length; i++) {
    const c = text.charCodeAt(i);
    if (c < 0x80) bytes.push(c);
    else if (c < 0x800) { bytes.push(0xc0 | (c >> 6), 0x80 | (c & 0x3f)); }
    else { bytes.push(0xe0 | (c >> 12), 0x80 | ((c >> 6) & 0x3f), 0x80 | (c & 0x3f)); }
  }
  const version = chooseVersion(bytes.length);
  const size = version * 4 + 17;

  // --- build bit buffer ---
  const buffer: number[] = [];
  let bitLen = 0;
  const put = (num: number, length: number) => {
    for (let i = 0; i < length; i++) {
      const bit = ((num >>> (length - i - 1)) & 1) === 1;
      const idx = bitLen >>> 3;
      if (buffer.length <= idx) buffer.push(0);
      if (bit) buffer[idx] |= 0x80 >>> (bitLen & 7);
      bitLen++;
    }
  };
  put(4, 4);                                 // byte mode indicator
  put(bytes.length, version < 10 ? 8 : 16);  // char count
  for (const b of bytes) put(b, 8);

  // RS block structure
  const layout = RS_M[version];
  const blocks: RsBlock[] = [];
  let totalData = 0;
  for (const [tc, dc] of layout) { blocks.push({ total: tc, data: [], ec: [] }); totalData += dc; }

  // terminator + pad to byte + pad bytes
  const dataBitCapacity = totalData * 8;
  if (bitLen + 4 <= dataBitCapacity) put(0, 4);
  while ((bitLen & 7) !== 0) { bitLen++; }
  while ((bitLen >>> 3) < totalData) { put(PAD0, 8); if ((bitLen >>> 3) < totalData) put(PAD1, 8); }

  // split data into blocks + compute EC
  let offset = 0;
  let maxDc = 0, maxEc = 0;
  for (let i = 0; i < blocks.length; i++) {
    const [tc, dc] = layout[i];
    const ecCount = tc - dc;
    maxDc = Math.max(maxDc, dc);
    maxEc = Math.max(maxEc, ecCount);
    const data = new Array(dc);
    for (let j = 0; j < dc; j++) data[j] = buffer[offset + j] & 0xff;
    offset += dc;
    const rs = rsPoly(ecCount);
    const raw = new Poly(data, rs.len() - 1);
    const mod = raw.mod(rs);
    const ec = new Array(ecCount);
    for (let j = 0; j < ecCount; j++) {
      const modIndex = j + mod.len() - ecCount;
      ec[j] = modIndex >= 0 ? mod.get(modIndex) : 0;
    }
    blocks[i].data = data;
    blocks[i].ec = ec;
  }

  // interleave
  const payload: number[] = [];
  for (let i = 0; i < maxDc; i++)
    for (const b of blocks) if (i < b.data.length) payload.push(b.data[i]);
  for (let i = 0; i < maxEc; i++)
    for (const b of blocks) if (i < b.ec.length) payload.push(b.ec[i]);

  // --- module matrix ---
  const modules: (boolean | null)[][] = Array.from({ length: size }, () => new Array(size).fill(null));

  const setupFinder = (row: number, col: number) => {
    for (let r = -1; r <= 7; r++) {
      if (row + r <= -1 || size <= row + r) continue;
      for (let c = -1; c <= 7; c++) {
        if (col + c <= -1 || size <= col + c) continue;
        const on = (0 <= r && r <= 6 && (c === 0 || c === 6)) ||
                   (0 <= c && c <= 6 && (r === 0 || r === 6)) ||
                   (2 <= r && r <= 4 && 2 <= c && c <= 4);
        modules[row + r][col + c] = on;
      }
    }
  };
  setupFinder(0, 0);
  setupFinder(size - 7, 0);
  setupFinder(0, size - 7);

  // timing
  for (let i = 8; i < size - 8; i++) {
    if (modules[i][6] === null) modules[i][6] = i % 2 === 0;
    if (modules[6][i] === null) modules[6][i] = i % 2 === 0;
  }

  // alignment patterns
  const ALIGN: Record<number, number[]> = {
    1: [], 2: [6, 18], 3: [6, 22], 4: [6, 26], 5: [6, 30],
    6: [6, 34], 7: [6, 22, 38], 8: [6, 24, 42], 9: [6, 26, 46], 10: [6, 28, 50],
  };
  const pos = ALIGN[version];
  for (const r of pos) for (const c of pos) {
    if (modules[r][c] !== null) continue;
    for (let dr = -2; dr <= 2; dr++)
      for (let dc2 = -2; dc2 <= 2; dc2++)
        modules[r + dr][c + dc2] = Math.max(Math.abs(dr), Math.abs(dc2)) !== 1;
  }

  // reserve format info area, place dark module
  modules[size - 8][8] = true;

  // place data with mask 0
  const mask = (r: number, c: number) => (r + c) % 2 === 0;
  let dataIndex = 0;
  let bitIndex = 7;
  let direction = -1;
  for (let col = size - 1; col > 0; col -= 2) {
    if (col === 6) col--;
    let row = direction === -1 ? size - 1 : 0;
    for (;;) {
      for (let c = 0; c < 2; c++) {
        const cc = col - c;
        if (modules[row][cc] === null) {
          let dark = false;
          if (dataIndex < payload.length) {
            dark = ((payload[dataIndex] >>> bitIndex) & 1) === 1;
          }
          if (mask(row, cc)) dark = !dark;
          modules[row][cc] = dark;
          bitIndex--;
          if (bitIndex === -1) { dataIndex++; bitIndex = 7; }
        }
      }
      row += direction;
      if (row < 0 || size <= row) { row -= direction; direction = -direction; break; }
    }
  }

  // format info (EC level M = 0b00, mask 0). type bits = (0b00 << 3) | 0 = 0
  const formatBits = bch15(0b00000);
  for (let i = 0; i < 15; i++) {
    const dark = ((formatBits >> i) & 1) === 1;
    if (i < 6) modules[i][8] = dark;
    else if (i < 8) modules[i + 1][8] = dark;
    else modules[size - 15 + i][8] = dark;
    if (i < 8) modules[8][size - i - 1] = dark;
    else if (i < 9) modules[8][15 - i - 1 + 1] = dark;
    else modules[8][15 - i - 1] = dark;
  }

  return modules.map((row) => row.map((m) => m === true));
}

export function qrSvg(text: string, opts: { size?: number; margin?: number } = {}): string {
  const matrix = makeQr(text);
  const n = matrix.length;
  const margin = opts.margin ?? 2;
  const total = n + margin * 2;
  const px = opts.size ?? 240;
  let rects = '';
  for (let r = 0; r < n; r++) {
    for (let c = 0; c < n; c++) {
      if (matrix[r][c]) rects += `<rect x="${c + margin}" y="${r + margin}" width="1" height="1"/>`;
    }
  }
  return `<svg xmlns="http://www.w3.org/2000/svg" width="${px}" height="${px}" viewBox="0 0 ${total} ${total}" shape-rendering="crispEdges"><rect width="${total}" height="${total}" fill="#fff"/><g fill="#0a0f1a">${rects}</g></svg>`;
}
