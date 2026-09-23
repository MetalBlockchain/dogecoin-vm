// The download page reads the release from the app's own signed update feed,
// so the version and size are always the current ones.
const $ = (id) => document.getElementById(id);

try {
  const xml = new DOMParser().parseFromString(await (await fetch('/download/macos/appcast.xml')).text(), 'application/xml');
  const item = xml.querySelector('item');
  const enclosure = item.querySelector('enclosure');
  const version = item.getElementsByTagName('sparkle:shortVersionString')[0]?.textContent
    || enclosure.getAttribute('sparkle:shortVersionString') || item.querySelector('title')?.textContent;
  $('dl-version').textContent = version || 'unknown';
  const date = new Date(item.querySelector('pubDate')?.textContent);
  $('dl-date').textContent = isNaN(date) ? 'unknown' : date.toLocaleDateString(undefined, { day: 'numeric', month: 'long', year: 'numeric' });
  const bytes = Number(enclosure.getAttribute('length'));
  $('dl-size').textContent = bytes ? `${(bytes / 1e6).toFixed(1)} MB` : 'unknown';
} catch {
  for (const id of ['dl-version', 'dl-date', 'dl-size']) $(id).textContent = 'unavailable';
}

try {
  const res = await fetch('/download/macos/DogecoinVM-Wallet.dmg.sha256');
  if (!res.ok) throw new Error();
  $('dl-sha').textContent = (await res.text()).trim().split(/\s+/)[0];
} catch {
  $('dl-sha').textContent = 'unavailable';
}
