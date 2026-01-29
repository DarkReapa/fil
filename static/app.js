const libraryEl = document.getElementById("library");
const uploadProgress = document.getElementById("uploadProgress");
const fileInput = document.getElementById("fileInput");
const browseBtn = document.getElementById("browse");
const dropzone = document.getElementById("dropzone");
const refreshBtn = document.getElementById("refresh");
const player = document.getElementById("player");
const nowPlaying = document.getElementById("nowPlaying");
const directUrl = document.getElementById("directUrl");
const playUrlBtn = document.getElementById("playUrl");
const streamName = document.getElementById("streamName");
const streamUrl = document.getElementById("streamUrl");
const addStreamBtn = document.getElementById("addStream");
const streamsEl = document.getElementById("streams");

let hls = null;

const formatSize = (size) => {
  if (!size) return "";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let idx = 0;
  let value = size;
  while (value > 1024 && idx < units.length - 1) {
    value /= 1024;
    idx += 1;
  }
  return `${value.toFixed(1)} ${units[idx]}`;
};

const setPlayerSource = (url, label) => {
  if (hls) {
    hls.destroy();
    hls = null;
  }

  if (Hls.isSupported() && url.endsWith(".m3u8")) {
    hls = new Hls();
    hls.loadSource(url);
    hls.attachMedia(player);
  } else {
    player.src = url;
  }

  nowPlaying.textContent = label || url;
  player.play().catch(() => {});
};

const renderLibrary = (items) => {
  libraryEl.innerHTML = "";
  if (!items.length) {
    libraryEl.innerHTML = "<p class=\"hint\">Пока нет загруженного контента.</p>";
    return;
  }

  items.forEach((item) => {
    const card = document.createElement("div");
    card.className = "library-item";
    card.innerHTML = `
      <div>
        <strong>${item.name}</strong><br />
        <small>${item.isDir ? "Каталог" : item.mimeType || "Файл"} · ${formatSize(item.size)}</small>
      </div>
      <button class="btn">Смотреть</button>
    `;

    if (!item.isDir) {
      card.querySelector("button").addEventListener("click", () => {
        const url = `/media/${item.path}`;
        setPlayerSource(url, item.name);
      });
    } else {
      card.querySelector("button").disabled = true;
    }
    libraryEl.appendChild(card);
  });
};

const loadLibrary = async () => {
  const res = await fetch("/api/library");
  const items = await res.json();
  renderLibrary(items);
};

const renderStreams = (streams) => {
  streamsEl.innerHTML = "";
  streams.forEach((stream) => {
    const card = document.createElement("div");
    card.className = "stream-card";
    card.innerHTML = `
      <div>
        <strong>${stream.name || "Без названия"}</strong><br />
        <small>${stream.url}</small>
      </div>
      <button class="btn">${stream.active ? "Смотреть" : "Запустить"}</button>
    `;
    card.querySelector("button").addEventListener("click", async () => {
      const res = await fetch(`/api/streams/${stream.id}/start`, { method: "POST" });
      const data = await res.json();
      if (data.url) {
        setPlayerSource(data.url, stream.name || stream.url);
      }
    });
    streamsEl.appendChild(card);
  });
};

const loadStreams = async () => {
  const res = await fetch("/api/streams");
  const streams = await res.json();
  renderStreams(streams);
};

const uploadFiles = async (files) => {
  if (!files.length) return;
  const form = new FormData();
  Array.from(files).forEach((file) => {
    form.append("files", file, file.webkitRelativePath || file.name);
  });

  uploadProgress.textContent = `Загружаем ${files.length} файлов...`;
  await fetch("/upload", { method: "POST", body: form });
  uploadProgress.textContent = "Загрузка завершена.";
  await loadLibrary();
};

browseBtn.addEventListener("click", () => fileInput.click());
fileInput.addEventListener("change", (event) => uploadFiles(event.target.files));
refreshBtn.addEventListener("click", loadLibrary);

["dragenter", "dragover"].forEach((eventName) => {
  dropzone.addEventListener(eventName, (event) => {
    event.preventDefault();
    dropzone.classList.add("active");
  });
});

["dragleave", "drop"].forEach((eventName) => {
  dropzone.addEventListener(eventName, (event) => {
    event.preventDefault();
    dropzone.classList.remove("active");
  });
});

dropzone.addEventListener("drop", (event) => {
  event.preventDefault();
  uploadFiles(event.dataTransfer.files);
});

playUrlBtn.addEventListener("click", () => {
  const url = directUrl.value.trim();
  if (url) {
    setPlayerSource(url, url);
  }
});

addStreamBtn.addEventListener("click", async () => {
  const name = streamName.value.trim();
  const url = streamUrl.value.trim();
  if (!url) return;
  await fetch("/api/streams", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ name, url }),
  });
  streamName.value = "";
  streamUrl.value = "";
  await loadStreams();
});

loadLibrary();
loadStreams();
