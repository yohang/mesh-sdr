# Accessibility checker (axe-core + Playwright Chromium), CI only. Node lives
# here and never in the app or dev image (ADR 0003, ADR 0007).
# Build from this directory: docker build .infra/a11y
FROM mcr.microsoft.com/playwright:v1.63.0-noble@sha256:eff16c30e6f3f4af0a03fa4b706120d5e9b0891c344a27d64559aff5900a4a27

WORKDIR /a11y

COPY package.json package-lock.json ./
RUN npm ci --no-audit --no-fund

COPY run.mjs urls.txt ./

USER pwuser

ENTRYPOINT ["node", "run.mjs"]
