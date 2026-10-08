// Drag-and-drop reordering of an admin table (ADM-021). The server renders
// the real controls: every row of a `tbody[data-msdr-sortable]` carries a
// `form[data-sort-drop]` with a `position` input (a 1-based target
// position), the same endpoint as the keyboard-accessible Up and Down
// buttons. Dropping a row sets the position and submits that form; htmx
// sends it (CSRF header included) and swaps the table. Without a pointer, the
// buttons do the same job.

const BODY = "tbody[data-msdr-sortable]";
const ROW = "tr[data-sort-id]";

let dragged = null;
let over = null;

function rowOf(node) {
  const row = node?.closest?.(ROW);
  return row && row.closest(BODY) ? row : null;
}

function clearOver() {
  over?.removeAttribute("data-drop");
  over = null;
}

function onDragStart(evt) {
  const row = rowOf(evt.target);
  if (!row) {
    return;
  }
  dragged = row;
  row.setAttribute("data-dragging", "");
  evt.dataTransfer.effectAllowed = "move";
  // Firefox starts no drag without data. Only the id of the row.
  evt.dataTransfer.setData("text/plain", row.dataset.sortId);
}

function onDragOver(evt) {
  const row = rowOf(evt.target);
  if (!dragged || !row || row.closest(BODY) !== dragged.closest(BODY)) {
    return;
  }
  evt.preventDefault();
  evt.dataTransfer.dropEffect = "move";
  if (row !== over) {
    clearOver();
    if (row !== dragged) {
      over = row;
      row.setAttribute("data-drop", "");
    }
  }
}

function onDrop(evt) {
  const row = rowOf(evt.target);
  const moved = dragged;
  if (!moved || !row || row === moved || row.closest(BODY) !== moved.closest(BODY)) {
    return;
  }
  evt.preventDefault();
  const rows = [...moved.closest(BODY).querySelectorAll(ROW)];
  const form = moved.querySelector("form[data-sort-drop]");
  const input = form?.querySelector('input[name="position"]');
  if (!form || !input) {
    return;
  }
  input.value = String(rows.indexOf(row) + 1);
  form.requestSubmit();
}

function onDragEnd() {
  dragged?.removeAttribute("data-dragging");
  dragged = null;
  clearOver();
}

document.addEventListener("dragstart", onDragStart);
document.addEventListener("dragover", onDragOver);
document.addEventListener("drop", onDrop);
document.addEventListener("dragend", onDragEnd);
