// Maidenhead grid overlay of the map (MAP-005, ADR 0029: drawn locally, no
// third-party fetch, no plugin): the lines and labels of the fields (20° by
// 10°, "JN") while zoomed out, of the squares (2° by 1°, "JN18") once they
// are wide enough to read. Redrawn for the visible area after every move.

const SQUARE_ZOOM = 6; // a square is about 90 px wide from zoom 6
const MAX_LABELS = 600;

/**
 * cellName is the locator of the field or square at a position.
 * @param {number} lat @param {number} lon @param {boolean} square
 */
function cellName(lat, lon, square) {
  const x = (((lon + 180) % 360) + 360) % 360;
  const y = Math.min(179.999, Math.max(0, lat + 90));
  const field = String.fromCharCode(65 + Math.floor(x / 20), 65 + Math.floor(y / 10));
  return square ? `${field}${Math.floor((x % 20) / 2)}${Math.floor(y % 10)}` : field;
}

/**
 * maidenheadGrid returns the grid layer.
 * @param {any} L Leaflet
 * @param {() => string} color the color of the lines
 */
export function maidenheadGrid(L, color) {
  const Grid = L.LayerGroup.extend({
    /** @param {any} map */
    onAdd(map) {
      L.LayerGroup.prototype.onAdd.call(this, map);
      map.on("moveend", this.redraw, this);
      this.redraw();
    },
    /** @param {any} map */
    onRemove(map) {
      map.off("moveend", this.redraw, this);
      L.LayerGroup.prototype.onRemove.call(this, map);
    },
    redraw() {
      const map = this._map;
      if (!map) return;
      this.clearLayers();
      const square = map.getZoom() >= SQUARE_ZOOM;
      const w = square ? 2 : 20;
      const h = square ? 1 : 10;
      const b = map.getBounds();
      const south = Math.max(-90, Math.floor(b.getSouth() / h) * h);
      const north = Math.min(90, Math.ceil(b.getNorth() / h) * h);
      const west = Math.floor(b.getWest() / w) * w;
      const east = Math.ceil(b.getEast() / w) * w;
      const line = { color: color(), weight: 1, opacity: 0.6, interactive: false };
      const field = { ...line, weight: 2 };
      for (let lon = west; lon <= east; lon += w) {
        this.addLayer(L.polyline([[south, lon], [north, lon]], lon % 20 === 0 ? field : line));
      }
      for (let lat = south; lat <= north; lat += h) {
        this.addLayer(L.polyline([[lat, west], [lat, east]], lat % 10 === 0 ? field : line));
      }
      if (((east - west) / w) * ((north - south) / h) > MAX_LABELS) return;
      for (let lon = west; lon < east; lon += w) {
        for (let lat = south; lat < north; lat += h) {
          const label = document.createElement("span");
          label.textContent = cellName(lat + h / 2, lon + w / 2, square);
          this.addLayer(
            L.marker([lat + h / 2, lon + w / 2], {
              icon: L.divIcon({ className: "map-grid-label", html: label, iconSize: null }),
              interactive: false,
              keyboard: false,
            }),
          );
        }
      }
    },
  });
  return new Grid();
}
