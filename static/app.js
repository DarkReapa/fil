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
const createRoomBtn = document.getElementById("createRoom");
const roomLinkInput = document.getElementById("roomLink");
const copyRoomBtn = document.getElementById("copyRoom");
const chatMessages = document.getElementById("chatMessages");
const chatNameInput = document.getElementById("chatName");
const chatTextInput = document.getElementById("chatText");
const sendChatBtn = document.getElementById("sendChat");
const roomStatus = document.getElementById("roomStatus");

let hls = null;
let currentSource = "";
let roomId = null;
let isHost = false;
let eventSource = null;
let syncTimer = null;
let isSyncing = false;

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

  currentSource = url;
  nowPlaying.textContent = label || url;
  player.play().catch(() => {});

  if (roomId && isHost) {
    sendRoomEvent({
      type: "sync",
      url,
      position: player.currentTime || 0,
      paused: player.paused,
    });
  }
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
    const downloadBtn = document.createElement("button");
    downloadBtn.className = "btn";
    downloadBtn.textContent = "Скачать";
    if (item.isDir) {
      downloadBtn.disabled = true;
    } else {
      downloadBtn.addEventListener("click", () => {
        window.open(`/download?path=${encodeURIComponent(item.path)}`, "_blank");
      });
    }
    card.appendChild(downloadBtn);
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
        <small>${stream.url} · ${stream.persist ? "Постоянный" : "Временный"}</small>
      </div>
      <button class="btn">${stream.active ? "Смотреть" : "Запустить"}</button>
    `;
    card.querySelector("button").addEventListener("click", async () => {
      const res = await fetch(`/api/streams/${stream.id}/start`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ persist: stream.persist }),
      });
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
  const persist = document.getElementById("persistStream").checked;
  await fetch("/api/streams", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ name, url, persist }),
  });
  streamName.value = "";
  streamUrl.value = "";
  document.getElementById("persistStream").checked = true;
  await loadStreams();
});

const appendChatMessage = (payload) => {
  const message = document.createElement("div");
  message.className = "chat-message";
  message.innerHTML = `
    <strong>${payload.user || "Гость"}</strong>
    <small>${payload.text}</small>
  `;
  chatMessages.appendChild(message);
  chatMessages.scrollTop = chatMessages.scrollHeight;
};

const sendRoomEvent = async (payload) => {
  if (!roomId) return;
  await fetch(`/api/rooms/${roomId}/events`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(payload),
  });
};

const connectRoom = () => {
  if (!roomId) return;
  if (eventSource) {
    eventSource.close();
  }
  eventSource = new EventSource(`/api/rooms/${roomId}/events`);
  eventSource.onmessage = (event) => {
    const data = JSON.parse(event.data);
    if (data.type === "chat") {
      appendChatMessage(data);
    }
    if (data.type === "sync" && !isHost) {
      isSyncing = true;
      if (data.url && data.url !== currentSource) {
        setPlayerSource(data.url, data.url);
      }
      if (typeof data.position === "number") {
        if (Math.abs(player.currentTime - data.position) > 1) {
          player.currentTime = data.position;
        }
      }
      if (data.paused) {
        player.pause();
      } else {
        player.play().catch(() => {});
      }
      isSyncing = false;
    }
  };
};

const initRoomFromUrl = async () => {
  const params = new URLSearchParams(window.location.search);
  const room = params.get("room");
  const host = params.get("host");
  if (!room) return;
  roomId = room;
  isHost = host === "1";
  roomStatus.textContent = `Комната ${roomId}`;
  connectRoom();

  const res = await fetch(`/api/rooms/${roomId}`);
  if (res.ok) {
    const state = await res.json();
    if (state.url) {
      setPlayerSource(state.url, state.name || state.url);
    }
  }
};

createRoomBtn.addEventListener("click", async () => {
  if (!currentSource) {
    alert("Сначала выберите фильм, серию или поток.");
    return;
  }
  const res = await fetch("/api/rooms", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ name: nowPlaying.textContent, url: currentSource }),
  });
  const room = await res.json();
  roomId = room.id;
  isHost = true;
  const link = `${window.location.origin}${window.location.pathname}?room=${roomId}`;
  roomLinkInput.value = link;
  roomStatus.textContent = `Комната ${roomId}`;
  connectRoom();
});

copyRoomBtn.addEventListener("click", async () => {
  if (!roomLinkInput.value) return;
  if (navigator.clipboard && navigator.clipboard.writeText) {
    await navigator.clipboard.writeText(roomLinkInput.value);
  } else {
    roomLinkInput.select();
    document.execCommand("copy");
  }
});

sendChatBtn.addEventListener("click", () => {
  const user = chatNameInput.value.trim() || "Гость";
  const text = chatTextInput.value.trim();
  if (!text || !roomId) return;
  sendRoomEvent({ type: "chat", user, text });
  chatTextInput.value = "";
});

player.addEventListener("timeupdate", () => {
  if (!roomId || !isHost || isSyncing) return;
  if (!syncTimer) {
    syncTimer = setTimeout(() => {
      sendRoomEvent({
        type: "sync",
        url: currentSource,
        position: player.currentTime,
        paused: player.paused,
      });
      syncTimer = null;
    }, 1000);
  }
});

player.addEventListener("play", () => {
  if (roomId && isHost && !isSyncing) {
    sendRoomEvent({
      type: "sync",
      url: currentSource,
      position: player.currentTime,
      paused: false,
    });
  }
});

player.addEventListener("pause", () => {
  if (roomId && isHost && !isSyncing) {
    sendRoomEvent({
      type: "sync",
      url: currentSource,
      position: player.currentTime,
      paused: true,
    });
  }
});

loadLibrary();
loadStreams();
initRoomFromUrl();
