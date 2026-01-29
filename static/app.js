const libraryEl = document.getElementById("library");
const uploadProgress = document.getElementById("uploadProgress");
const uploadList = document.getElementById("uploadList");
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
const chatOverlay = document.getElementById("chatOverlay");
const chatPanel = document.getElementById("chatPanel");
const toggleChatBtn = document.getElementById("toggleChat");
const chatOpacityInput = document.getElementById("chatOpacity");
const deleteRoomBtn = document.getElementById("deleteRoom");
const chatFullscreenMessages = document.getElementById("chatFullscreenMessages");
const chatFullscreenText = document.getElementById("chatFullscreenText");
const sendChatFullscreenBtn = document.getElementById("sendChatFullscreen");
const toggleFullscreenChatBtn = document.getElementById("toggleFullscreenChat");
const chatToast = document.getElementById("chatToast");
const chatFullscreen = document.getElementById("chatFullscreen");
const searchQuery = document.getElementById("searchQuery");
const searchTags = document.getElementById("searchTags");
const searchGenres = document.getElementById("searchGenres");
const applySearchBtn = document.getElementById("applySearch");
const resetSearchBtn = document.getElementById("resetSearch");

let hls = null;
let playerInstance = null;
let currentSource = "";
let roomId = null;
let isHost = false;
let eventSource = null;
let isSyncing = false;
let isChatCollapsed = false;
let isFullscreenChatCollapsed = true;

const uploads = new Map();
const chunkSize = 5 * 1024 * 1024;

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
        <strong>${item.title || item.name}</strong><br />
        <small>${item.isDir ? "Каталог" : item.mimeType || "Файл"} · ${formatSize(item.size)}</small>
        <div class="meta-edit">
          <input type="text" class="meta-title" placeholder="Название" value="${item.title || ""}" />
          <input type="text" class="meta-tags" placeholder="Теги через запятую" value="${(item.tags || []).join(", ")}" />
          <input type="text" class="meta-genres" placeholder="Жанры через запятую" value="${(item.genres || []).join(", ")}" />
        </div>
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
    const saveBtn = document.createElement("button");
    saveBtn.className = "btn primary";
    saveBtn.textContent = "Сохранить";
    saveBtn.addEventListener("click", async () => {
      const title = card.querySelector(".meta-title").value.trim();
      const tags = card
        .querySelector(".meta-tags")
        .value.split(",")
        .map((value) => value.trim())
        .filter(Boolean);
      const genres = card
        .querySelector(".meta-genres")
        .value.split(",")
        .map((value) => value.trim())
        .filter(Boolean);
      await fetch("/api/library/item", {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ path: item.path, title, tags, genres }),
      });
      await loadLibrary();
    });
    card.appendChild(saveBtn);
    const deleteBtn = document.createElement("button");
    deleteBtn.className = "btn";
    deleteBtn.textContent = "Удалить";
    deleteBtn.addEventListener("click", async () => {
      await fetch(`/api/library/item?path=${encodeURIComponent(item.path)}`, {
        method: "DELETE",
      });
      await loadLibrary();
    });
    card.appendChild(deleteBtn);
    libraryEl.appendChild(card);
  });
};

const isNearBottom = (element) => {
  const threshold = 40;
  return element.scrollHeight - element.scrollTop - element.clientHeight < threshold;
};

const loadLibrary = async (queryParams = "") => {
  const res = await fetch(`/api/library${queryParams}`);
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
      <div class="stream-actions">
        <button class="btn">${stream.active ? "Смотреть" : "Запустить"}</button>
        <button class="btn">Удалить</button>
      </div>
    `;
    const actionButtons = card.querySelectorAll("button");
    actionButtons[0].addEventListener("click", async () => {
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
    actionButtons[1].addEventListener("click", async () => {
      await fetch(`/api/streams/${stream.id}`, { method: "DELETE" });
      await loadStreams();
    });
    streamsEl.appendChild(card);
  });
};

const loadStreams = async () => {
  const res = await fetch("/api/streams");
  const streams = await res.json();
  renderStreams(streams);
};

const createUploadItem = (file) => {
  const item = document.createElement("div");
  item.className = "upload-item";
  const header = document.createElement("div");
  header.className = "upload-item-header";
  const name = document.createElement("span");
  name.textContent = file.name;
  const status = document.createElement("span");
  status.textContent = "Ожидание";
  header.appendChild(name);
  header.appendChild(status);

  const bar = document.createElement("div");
  bar.className = "upload-bar";
  const barFill = document.createElement("span");
  bar.appendChild(barFill);

  const actions = document.createElement("div");
  actions.className = "upload-actions";
  const resumeBtn = document.createElement("button");
  resumeBtn.className = "btn";
  resumeBtn.textContent = "Возобновить";
  resumeBtn.disabled = true;
  const cancelBtn = document.createElement("button");
  cancelBtn.className = "btn danger";
  cancelBtn.textContent = "Отменить";
  actions.appendChild(resumeBtn);
  actions.appendChild(cancelBtn);

  item.appendChild(header);
  item.appendChild(bar);
  item.appendChild(actions);
  uploadList.appendChild(item);

  return { item, status, barFill, resumeBtn, cancelBtn };
};

const updateUploadProgress = (entry) => {
  const percent = Math.min(100, Math.floor((entry.offset / entry.size) * 100));
  entry.ui.barFill.style.width = `${percent}%`;
  entry.ui.status.textContent = `${percent}%`;
};

const uploadChunk = async (entry) => {
  if (entry.controller) {
    entry.controller.abort();
  }
  entry.controller = new AbortController();
  const end = Math.min(entry.offset + chunkSize, entry.size);
  const chunk = entry.file.slice(entry.offset, end);
  const res = await fetch(`/api/upload/chunk?id=${encodeURIComponent(entry.id)}`, {
    method: "PUT",
    headers: { "X-Upload-Offset": entry.offset.toString() },
    body: chunk,
    signal: entry.controller.signal,
  });
  if (!res.ok) {
    throw new Error("upload failed");
  }
  const data = await res.json();
  entry.offset = data.offset || entry.offset + chunk.size;
  updateUploadProgress(entry);
};

const runUpload = async (entry) => {
  entry.ui.status.textContent = "Загрузка...";
  entry.ui.resumeBtn.disabled = true;
  try {
    while (entry.offset < entry.size) {
      await uploadChunk(entry);
    }
    entry.ui.status.textContent = "Готово";
    entry.ui.cancelBtn.disabled = true;
    await loadLibrary();
  } catch (error) {
    if (!entry.canceled) {
      entry.ui.status.textContent = "Пауза";
      entry.ui.resumeBtn.disabled = false;
    }
  }
};

const startUpload = async (entry) => {
  if (!entry.id) {
    const res = await fetch("/api/upload/start", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ path: entry.path, size: entry.size }),
    });
    const data = await res.json();
    entry.id = data.id;
  }
  const statusRes = await fetch(`/api/upload/status?id=${encodeURIComponent(entry.id)}`);
  if (statusRes.ok) {
    const status = await statusRes.json();
    entry.offset = status.offset || 0;
  }
  updateUploadProgress(entry);
  await runUpload(entry);
};

const uploadFiles = async (files) => {
  if (!files.length) return;
  uploadProgress.textContent = `Загружаем ${files.length} файлов...`;
  Array.from(files).forEach((file) => {
    const path = file.webkitRelativePath || file.name;
    const ui = createUploadItem(file);
    const entry = {
      file,
      path,
      size: file.size,
      offset: 0,
      id: null,
      canceled: false,
      controller: null,
      ui,
    };
    uploads.set(path, entry);

    ui.resumeBtn.addEventListener("click", () => {
      startUpload(entry);
    });
    ui.cancelBtn.addEventListener("click", async () => {
      entry.canceled = true;
      if (entry.controller) {
        entry.controller.abort();
      }
      if (entry.id) {
        await fetch(`/api/upload/cancel?id=${encodeURIComponent(entry.id)}`, { method: "POST" });
      }
      entry.ui.status.textContent = "Отменено";
      entry.ui.resumeBtn.disabled = true;
      entry.ui.cancelBtn.disabled = true;
    });

    startUpload(entry);
  });
};

browseBtn.addEventListener("click", () => fileInput.click());
fileInput.addEventListener("change", (event) => uploadFiles(event.target.files));
refreshBtn.addEventListener("click", loadLibrary);
applySearchBtn.addEventListener("click", () => {
  const params = new URLSearchParams();
  if (searchQuery.value.trim()) params.set("q", searchQuery.value.trim());
  if (searchTags.value.trim()) params.set("tags", searchTags.value.trim());
  if (searchGenres.value.trim()) params.set("genres", searchGenres.value.trim());
  const query = params.toString();
  loadLibrary(query ? `?${query}` : "");
});

resetSearchBtn.addEventListener("click", () => {
  searchQuery.value = "";
  searchTags.value = "";
  searchGenres.value = "";
  loadLibrary();
});

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

const showChatToast = () => {
  if (!chatToast) return;
  chatToast.classList.add("visible");
  clearTimeout(chatToast.timeoutId);
  chatToast.timeoutId = setTimeout(() => {
    chatToast.classList.remove("visible");
  }, 3500);
};

const lockChatName = () => {
  if (!roomId) return;
  const user = chatNameInput.value.trim();
  if (!user || chatNameInput.disabled) return;
  localStorage.setItem(`room-name-${roomId}`, user);
  chatNameInput.disabled = true;
};

const appendChatMessage = (payload, shouldScroll = true) => {
  const autoScroll = shouldScroll && isNearBottom(chatMessages);
  const message = document.createElement("div");
  message.className = "chat-message";
  message.innerHTML = `
    <strong>${payload.user || "Гость"}</strong>
    <small>${payload.text}</small>
  `;
  chatMessages.appendChild(message);
  if (autoScroll) {
    chatMessages.scrollTop = chatMessages.scrollHeight;
  }

  if (document.fullscreenElement) {
    const overlayMessage = document.createElement("div");
    overlayMessage.className = "chat-overlay-message";
    overlayMessage.textContent = `${payload.user || "Гость"}: ${payload.text}`;
    overlayMessage.style.opacity = getComputedStyle(chatPanel).getPropertyValue("--chat-opacity");
    chatOverlay.appendChild(overlayMessage);
    setTimeout(() => {
      overlayMessage.remove();
    }, 6000);
  }

  const fullscreenMessage = document.createElement("div");
  fullscreenMessage.className = "chat-fullscreen-message";
  fullscreenMessage.innerHTML = `<strong>${payload.user || "Гость"}</strong><span>${payload.text}</span>`;
  chatFullscreenMessages.appendChild(fullscreenMessage);
  chatFullscreenMessages.scrollTop = chatFullscreenMessages.scrollHeight;

  const shouldNotify =
    chatPanel.classList.contains("chat-hidden") ||
    chatPanel.classList.contains("chat-collapsed") ||
    chatFullscreen.classList.contains("chat-fullscreen-collapsed");
  if (shouldNotify) {
    showChatToast();
  }
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
    if (data.type === "history") {
      chatMessages.innerHTML = "";
      chatFullscreenMessages.innerHTML = "";
      (data.messages || []).forEach((message) => appendChatMessage(message, false));
      chatMessages.scrollTop = chatMessages.scrollHeight;
      return;
    }
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
  if (!room) return;
  roomId = room;
  isHost = localStorage.getItem(`room-host-${roomId}`) === "1";
  roomStatus.textContent = `Комната ${roomId}`;
  chatPanel.classList.remove("chat-hidden");
  chatFullscreen.classList.remove("chat-hidden");
  deleteRoomBtn.disabled = false;
  sendChatBtn.disabled = false;
  sendChatFullscreenBtn.disabled = false;
  connectRoom();

  const res = await fetch(`/api/rooms/${roomId}`);
  if (res.ok) {
    const state = await res.json();
    if (state.url) {
      setPlayerSource(state.url, state.name || state.url);
    }
  }
  const savedName = localStorage.getItem(`room-name-${roomId}`);
  if (savedName) {
    chatNameInput.value = savedName;
    chatNameInput.disabled = true;
  } else {
    chatNameInput.disabled = false;
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
  localStorage.setItem(`room-host-${roomId}`, "1");
  chatPanel.classList.remove("chat-hidden");
  chatFullscreen.classList.remove("chat-hidden");
  deleteRoomBtn.disabled = false;
  sendChatBtn.disabled = false;
  sendChatFullscreenBtn.disabled = false;
  const link = `${window.location.origin}${window.location.pathname}?room=${roomId}`;
  roomLinkInput.value = link;
  roomStatus.textContent = `Комната ${roomId}`;
  connectRoom();
  chatNameInput.disabled = false;
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

const sendChatMessage = () => {
  const user = chatNameInput.value.trim() || "Гость";
  const text = chatTextInput.value.trim() || chatFullscreenText.value.trim();
  if (!text || !roomId) return;
  if (user !== "Гость") {
    chatNameInput.value = user;
    lockChatName();
  }
  sendRoomEvent({ type: "chat", user, text });
  chatTextInput.value = "";
  chatFullscreenText.value = "";
};

sendChatBtn.addEventListener("click", sendChatMessage);
sendChatFullscreenBtn.addEventListener("click", sendChatMessage);

chatNameInput.addEventListener("blur", lockChatName);

toggleChatBtn.addEventListener("click", () => {
  isChatCollapsed = !isChatCollapsed;
  chatPanel.classList.toggle("chat-collapsed", isChatCollapsed);
  toggleChatBtn.textContent = isChatCollapsed ? "Развернуть" : "Свернуть";
});

toggleFullscreenChatBtn.addEventListener("click", () => {
  isFullscreenChatCollapsed = !isFullscreenChatCollapsed;
  chatFullscreen.classList.toggle("chat-fullscreen-collapsed", isFullscreenChatCollapsed);
  toggleFullscreenChatBtn.textContent = isFullscreenChatCollapsed ? "›" : "‹";
});

toggleFullscreenChatBtn.textContent = "›";

chatOpacityInput.addEventListener("input", () => {
  const value = chatOpacityInput.value;
  chatPanel.style.setProperty("--chat-opacity", value);
  chatFullscreen.style.opacity = value;
  localStorage.setItem("chat-opacity", value);
});

deleteRoomBtn.addEventListener("click", async () => {
  if (!roomId) return;
  await fetch(`/api/rooms/${roomId}`, { method: "DELETE" });
  localStorage.removeItem(`room-host-${roomId}`);
  localStorage.removeItem(`room-name-${roomId}`);
  roomStatus.textContent = "Комната не выбрана";
  roomId = null;
  isHost = false;
  chatPanel.classList.add("chat-hidden");
  chatFullscreen.classList.add("chat-hidden");
  deleteRoomBtn.disabled = true;
  sendChatBtn.disabled = true;
  sendChatFullscreenBtn.disabled = true;
  roomLinkInput.value = "";
  chatMessages.innerHTML = "";
  chatFullscreenMessages.innerHTML = "";
  chatFullscreenText.value = "";
  chatNameInput.disabled = false;
  chatNameInput.value = "";
  if (eventSource) {
    eventSource.close();
  }
});

const applySavedOpacity = () => {
  const saved = localStorage.getItem("chat-opacity");
  if (saved) {
    chatOpacityInput.value = saved;
    chatPanel.style.setProperty("--chat-opacity", saved);
    chatFullscreen.style.opacity = saved;
  } else {
    chatOpacityInput.value = "1";
    chatPanel.style.setProperty("--chat-opacity", "1");
    chatFullscreen.style.opacity = "1";
  }
};


const emitSync = () => {
  if (!roomId || !isHost || isSyncing) return;
  sendRoomEvent({
    type: "sync",
    url: currentSource,
    position: player.currentTime,
    paused: player.paused,
  });
};

player.addEventListener("play", () => {
  emitSync();
});

player.addEventListener("pause", () => {
  emitSync();
});

player.addEventListener("seeked", () => {
  emitSync();
});

playerInstance = new Plyr(player, {
  controls: [
    "play-large",
    "play",
    "progress",
    "current-time",
    "mute",
    "volume",
    "captions",
    "settings",
    "pip",
    "airplay",
    "fullscreen",
  ],
});

if (playerInstance && playerInstance.elements && playerInstance.elements.container) {
  playerInstance.elements.container.appendChild(chatFullscreen);
  playerInstance.on("enterfullscreen", () => {
    playerInstance.elements.container.classList.add("fullscreen-active");
  });
  playerInstance.on("exitfullscreen", () => {
    playerInstance.elements.container.classList.remove("fullscreen-active");
  });
}

loadLibrary();
loadStreams();
applySavedOpacity();
deleteRoomBtn.disabled = true;
sendChatBtn.disabled = true;
sendChatFullscreenBtn.disabled = true;
initRoomFromUrl();
