// The address book on the wallet page: a picker on the address fields, an
// offer to save a new address, and the book itself on the Addresses tab.
// The book's rules are in addressbook.js.
//
// Names are the viewer's own text: they are only ever set as textContent.

import * as book from './addressbook.js';

const STORE = 'dogevm.addressbook';
const SHOWN = 8; // options listed at once while typing

// createAddressBook wires the book into the page. store is app.js's
// storage wrapper; decode(address, network) returns an address's canonical
// text or throws. Storage may be unavailable: the book then lasts until the
// page is closed, and says so.
export function createAddressBook({ $, store, decode }) {
  let list = book.parse(store.get(STORE), decode);
  const pickers = [];

  function save(next) {
    list = next;
    const kept = store.set(STORE, book.serialize(list));
    const warning = $('ab-warning');
    warning.hidden = kept;
    warning.textContent = kept ? '' : "This browser won't keep your address book: it lasts until you close the page.";
    renderManager();
    for (const p of pickers) p.refresh();
  }

  // lookup returns what the book knows of an address on a network:
  // {name, network, sameNetwork}, or null. The address may be as typed.
  function lookup(address, network) {
    let text;
    try { text = decode(String(address).trim(), network); } catch { return null; }
    const e = book.find(list, text, network);
    return e && { name: e.name, network: e.network, sameNetwork: e.network === network };
  }

  // --- the book, on the Addresses tab -----------------------------------------

  // What a row is doing: null, or {address, network, mode: 'rename'|'delete'}.
  let editing = null;
  let focusAfter = null; // selector to focus once the list is redrawn

  const entryKey = (e) => `${e.network}:${e.address}`;
  const button = (text, cls, run) => {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = cls;
    b.textContent = text;
    b.addEventListener('click', run);
    return b;
  };
  const span = (cls, text) => Object.assign(document.createElement('span'), { className: cls, textContent: text });

  function renderManager() {
    $('ab-count').textContent = list.length ? `(${list.length})` : '';
    const sorted = [...list].sort(book.byName);
    if (sorted.length === 0) {
      const li = document.createElement('li');
      li.className = 'empty';
      li.textContent = 'No saved addresses yet. Add one below, or save one as you type it on the Send or Withdraw tab.';
      $('ab-list').replaceChildren(li);
    } else {
      $('ab-list').replaceChildren(...sorted.map(row));
    }
    if (focusAfter) {
      const el = $('ab-list').querySelector(focusAfter) || $('ab-add-name');
      focusAfter = null;
      el.focus();
    }
  }

  function row(e) {
    const li = document.createElement('li');
    li.dataset.key = entryKey(e);
    const mode = editing && editing.address === e.address && editing.network === e.network ? editing.mode : null;
    const text = document.createElement('div');
    text.className = 'ab-entry';
    text.append(span('ab-name', e.name), ' ', span(`ab-network ab-${e.network}`, book.NETWORKS[e.network]),
      span('ab-address mono', e.address));
    li.append(text);
    const actions = document.createElement('div');
    actions.className = 'ab-actions';
    const close = (sel) => { editing = null; focusAfter = sel; renderManager(); };
    const at = `[data-key="${CSS.escape(entryKey(e))}"]`;

    if (mode === 'rename') {
      const label = document.createElement('label');
      label.className = 'visually-hidden';
      label.htmlFor = 'ab-rename';
      label.textContent = `New name for ${e.address}`;
      const input = Object.assign(document.createElement('input'), {
        id: 'ab-rename', value: e.name, maxLength: book.MAX_NAME, autocomplete: 'off', className: 'ab-name-input',
      });
      const error = span('ab-error', '');
      error.setAttribute('role', 'alert');
      const commit = () => {
        try {
          const next = book.rename(list, e.address, e.network, input.value);
          editing = null;
          focusAfter = `${at} .ab-rename-button`;
          save(next);
        } catch (err) { error.textContent = err.message; }
      };
      input.addEventListener('keydown', (ev) => {
        if (ev.key === 'Enter') { ev.preventDefault(); commit(); }
        if (ev.key === 'Escape') { ev.preventDefault(); close(`${at} .ab-rename-button`); }
      });
      actions.append(label, input, button('Save', 'primary', commit),
        button('Cancel', '', () => close(`${at} .ab-rename-button`)), error);
    } else if (mode === 'delete') {
      // Asked here, in the page: no browser dialogs.
      const q = document.createElement('p');
      q.className = 'ab-confirm';
      q.append('Delete "', span('', e.name), '" from the address book?');
      actions.append(q,
        button('Delete', 'danger ab-delete-confirm', () => {
          const sorted = [...list].sort(book.byName);
          const next = sorted[sorted.findIndex((x) => entryKey(x) === entryKey(e)) + 1];
          editing = null;
          focusAfter = next ? `[data-key="${CSS.escape(entryKey(next))}"] .ab-delete-button` : '#ab-add-name';
          save(book.remove(list, e.address, e.network));
        }),
        button('Keep it', 'ab-keep', () => close(`${at} .ab-delete-button`)));
    } else {
      const rn = button('Rename', 'small ab-rename-button', () => {
        editing = { address: e.address, network: e.network, mode: 'rename' };
        focusAfter = '#ab-rename';
        renderManager();
      });
      rn.setAttribute('aria-label', `Rename ${e.name}`);
      const del = button('Delete', 'small ab-delete-button', () => {
        editing = { address: e.address, network: e.network, mode: 'delete' };
        focusAfter = `${at} .ab-keep`;
        renderManager();
      });
      del.setAttribute('aria-label', `Delete ${e.name}`);
      actions.append(rn, del);
    }
    li.append(actions);
    return li;
  }

  $('ab-add').addEventListener('submit', (ev) => {
    ev.preventDefault();
    const result = $('ab-add-result');
    try {
      const network = $('ab-add').querySelector('input[name=ab-add-network]:checked').value;
      const next = book.add(list, { name: $('ab-add-name').value, address: $('ab-add-address').value, network }, decode);
      save(next);
      result.textContent = 'Saved.';
      result.className = 'result ok';
      $('ab-add-name').value = '';
      $('ab-add-address').value = '';
    } catch (err) {
      result.textContent = err.message;
      result.className = 'result error';
    }
  });

  // --- the picker and "Save this address", on an address field ---------------

  // attach adds the book to an address field. network() is the network the
  // field pays on; only limits the picker to that network (a withdrawal pays
  // Dogecoin); onPick runs after an entry is picked.
  function attach({ input, network, only = false, onPick = () => {} }) {
    const id = input.id;
    const listbox = $(`${id}-book`);
    const note = $(`${id}-saved`);
    const offer = $(`${id}-save`);
    const form = $(`${id}-save-form`);
    const nameInput = $(`${id}-save-name`);
    const netSelect = $(`${id}-save-network`); // absent where the network is fixed
    const result = $(`${id}-save-result`);
    let shown = [];
    let active = -1;

    input.setAttribute('role', 'combobox');
    input.setAttribute('aria-autocomplete', 'list');
    input.setAttribute('aria-controls', listbox.id);
    input.setAttribute('aria-expanded', 'false');

    function close() {
      listbox.hidden = true;
      input.setAttribute('aria-expanded', 'false');
      input.removeAttribute('aria-activedescendant');
      active = -1;
    }

    function open() {
      const net = network();
      let valid = false;
      try { decode(input.value.trim(), net); valid = true; } catch { /* still typing */ }
      // A complete address needs no suggestions.
      shown = valid ? [] : book.matches(list, input.value, only ? { only: net } : { prefer: net }).slice(0, SHOWN);
      if (shown.length === 0 || document.activeElement !== input) { close(); return; }
      active = Math.min(active, shown.length - 1);
      listbox.replaceChildren(...shown.map((e, i) => {
        const li = document.createElement('li');
        li.id = `${listbox.id}-${i}`;
        li.setAttribute('role', 'option');
        li.setAttribute('aria-selected', String(i === active));
        li.append(span('ab-name', e.name), span(`ab-network ab-${e.network}`, book.NETWORKS[e.network]),
          span('ab-short mono', book.shorten(e.address)));
        // Keep focus in the field, so the list doesn't close before a click.
        li.addEventListener('mousedown', (ev) => ev.preventDefault());
        li.addEventListener('click', () => pick(e));
        return li;
      }));
      listbox.hidden = false;
      input.setAttribute('aria-expanded', 'true');
      if (active >= 0) input.setAttribute('aria-activedescendant', `${listbox.id}-${active}`);
      else input.removeAttribute('aria-activedescendant');
    }

    function pick(e) {
      input.value = e.address;
      close();
      onPick(e);
      refresh();
    }

    function move(by) {
      if (listbox.hidden) { active = -1; open(); if (listbox.hidden) return; }
      active = (active + by + shown.length) % shown.length;
      for (const [i, li] of [...listbox.children].entries()) li.setAttribute('aria-selected', String(i === active));
      input.setAttribute('aria-activedescendant', `${listbox.id}-${active}`);
      listbox.children[active].scrollIntoView({ block: 'nearest' });
    }

    // refresh says whether what is typed is saved, and offers to save it if
    // it's a valid address that isn't.
    function refresh() {
      const net = network();
      let address = null;
      try { address = decode(input.value.trim(), net); } catch { /* not an address yet */ }
      const saved = address && book.find(list, address, net);
      note.hidden = !saved;
      note.className = saved && saved.network !== net ? 'ab-note warn' : 'ab-note';
      note.textContent = !saved ? ''
        : saved.network === net ? `Saved as "${saved.name}".`
          : `Saved as "${saved.name}", for ${book.NETWORKS[saved.network]}. This payment is on ${book.NETWORKS[net]}.`;
      const canSave = Boolean(address) && !(saved && saved.network === net);
      offer.hidden = !canSave;
      if (!canSave) {
        form.hidden = true;
        $(`${id}-save-open`).hidden = false;
      }
      if (netSelect && form.hidden) netSelect.value = net;
    }

    input.addEventListener('input', () => {
      active = -1;
      result.textContent = '';
      open();
      refresh();
    });
    input.addEventListener('focus', open);
    input.addEventListener('blur', close);
    input.addEventListener('keydown', (ev) => {
      if (ev.key === 'ArrowDown') { ev.preventDefault(); move(1); }
      else if (ev.key === 'ArrowUp') { ev.preventDefault(); move(-1); }
      else if (ev.key === 'Enter' && !listbox.hidden && active >= 0) { ev.preventDefault(); pick(shown[active]); }
      else if (ev.key === 'Escape' && !listbox.hidden) { ev.preventDefault(); close(); }
    });

    $(`${id}-save-open`).addEventListener('click', () => {
      $(`${id}-save-open`).hidden = true;
      form.hidden = false;
      result.textContent = '';
      nameInput.focus();
    });
    const cancel = () => {
      form.hidden = true;
      $(`${id}-save-open`).hidden = false;
      nameInput.value = '';
      $(`${id}-save-open`).focus();
    };
    const commit = () => {
      try {
        const net = netSelect ? netSelect.value : network();
        const next = book.add(list, { name: nameInput.value, address: input.value, network: net }, decode);
        nameInput.value = '';
        form.hidden = true;
        $(`${id}-save-open`).hidden = false;
        save(next);
        result.textContent = `Saved to your address book, for ${book.NETWORKS[net]}.`;
        result.className = 'ab-save-result ok';
        input.focus();
      } catch (err) {
        result.textContent = err.message;
        result.className = 'ab-save-result error';
      }
    };
    // These fields sit inside a payment form: Enter here saves the name, it
    // must never submit the payment.
    nameInput.addEventListener('keydown', (ev) => {
      if (ev.key === 'Enter') { ev.preventDefault(); commit(); }
      if (ev.key === 'Escape') { ev.preventDefault(); cancel(); }
    });
    $(`${id}-save-confirm`).addEventListener('click', commit);
    $(`${id}-save-cancel`).addEventListener('click', cancel);

    const picker = {
      refresh,
      clear() { result.textContent = ''; close(); refresh(); },
    };
    pickers.push(picker);
    refresh();
    return picker;
  }

  renderManager();
  return { lookup, attach };
}
