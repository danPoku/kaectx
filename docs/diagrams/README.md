# Diagrams

Each diagram has an HTML source and a PNG export, in a light and a dark version.
GitHub can't render the HTML inside a README, so the README shows the PNGs and
picks one with `<picture>` according to the reader's colour scheme.

| Source | Export | Used in |
| --- | --- | --- |
| `architecture.html` | `architecture.png` | README, under the introduction |
| `architecture-dark.html` | `architecture-dark.png` | Same place, dark mode |

The sources were drawn with the
[diagram-design](https://github.com/cathrynlavery/diagram-design) agent skill.
Each one is a single HTML file with an inline SVG, and opens straight in a
browser.

## Changing a diagram

1. Edit the SVG in **both** HTML files. They have the same layout and differ
   only in colour, so a node moved in one has to move in the other.
2. Open them in a browser and check them.
3. Export the PNGs again. One way, with Chrome: make a copy of the HTML that
   hides the title and sets the `<svg>` to its `viewBox` size (1152×488), then
   run

   ```
   chrome --headless=new --hide-scrollbars --force-device-scale-factor=2 \
     --window-size=1152,488 --virtual-time-budget=8000 \
     --screenshot=architecture.png file:///path/to/copy.html
   ```

   The skill's own `/export-diagram` command does the same through Playwright.

Update the diagram when the thing it shows changes: a new agent adapter, a new
store, or a different way of serving results. A wrong diagram is worse than none.
