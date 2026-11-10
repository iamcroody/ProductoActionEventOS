// Print only the element marked .print-region (QR + URL), hiding the rest of the
// app. A body class scopes the print rules so the existing full-page printing
// (e.g. Dashboard) keeps working unchanged.
export function printRegion() {
  document.body.classList.add('printing-region');
  const cleanup = () => {
    document.body.classList.remove('printing-region');
    window.removeEventListener('afterprint', cleanup);
  };
  window.addEventListener('afterprint', cleanup);
  window.print();
  // Fallback for browsers that don't fire afterprint reliably.
  setTimeout(cleanup, 1500);
}
