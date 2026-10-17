FROM node:22-bookworm-slim@sha256:c3de60bf2f9dd0ac6370e6117950ff62d6e339527e7472301c9c78a017978392
WORKDIR /srv/bps-login/runtime
COPY runtime/package.json runtime/package-lock.json ./
RUN npm ci --omit=dev --no-audit --no-fund
WORKDIR /srv/bps-login
COPY package.json ./
COPY runtime/ ./runtime/
COPY upstream/ ./upstream/
COPY src/ ./src/
COPY licenses/ ./licenses/
COPY UPSTREAM.md README.md ./
# Restore the original trust bundle required by native Go TLS.
COPY --from=node:22-bookworm@sha256:363e1587494626837fa7f9a23bdb453d13b0ff3c67c705c2805cfc69c2d2fad7 /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
ENV NODE_ENV=production
EXPOSE 8001
HEALTHCHECK --interval=15s --timeout=3s --start-period=15s CMD node -e "require('http').get('http://127.0.0.1:8001/healthz',r=>{let b='';r.on('data',c=>b+=c);r.on('end',()=>{try{process.exit(JSON.parse(b).ready===true?0:1)}catch{process.exit(1)}})}).on('error',()=>process.exit(1))"
CMD ["node","src/main.js"]
