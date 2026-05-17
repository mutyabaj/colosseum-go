FROM node:20-slim
WORKDIR /app
COPY package*.json ./
RUN npm ci --only=production
COPY index.js ./
RUN mkdir -p /tmp/zernio-uploads
EXPOSE 3001
CMD ["node", "index.js"]
