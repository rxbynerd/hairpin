(function () {
  "use strict";

  var feed = document.getElementById("event-feed");
  if (!feed || feed.dataset.terminal === "true") {
    return;
  }

  var jobId = feed.dataset.jobId;
  var userScrolled = false;

  feed.addEventListener("scroll", function () {
    var atBottom = feed.scrollHeight - feed.scrollTop - feed.clientHeight < 24;
    userScrolled = !atBottom;
  });

  function scrollToEnd() {
    if (!userScrolled) {
      feed.scrollTop = feed.scrollHeight;
    }
  }

  function appendLine(cls, text) {
    var line = document.createElement("div");
    line.className = "event-line " + cls;
    line.textContent = text;
    feed.appendChild(line);
    scrollToEnd();
  }

  function appendText(text) {
    var last = feed.lastElementChild;
    if (!last || !last.classList.contains("event-text")) {
      last = document.createElement("div");
      last.className = "event-line event-text";
      feed.appendChild(last);
    }
    last.textContent += text;
    scrollToEnd();
  }

  function reload() {
    window.location.reload();
  }

  var source = new EventSource("/jobs/" + jobId + "/events");

  source.addEventListener("text_delta", function (e) {
    try {
      var payload = JSON.parse(e.data);
      appendText(payload.text || payload.delta || "");
    } catch (err) {
      appendText(e.data);
    }
  });

  ["tool_call", "tool_result", "status_change", "error"].forEach(function (type) {
    source.addEventListener(type, function (e) {
      appendLine("event-" + type, type + ": " + e.data);
    });
  });

  source.addEventListener("permission_request", reload);
  source.addEventListener("done", reload);

  source.addEventListener("eof", function () {
    source.close();
  });

  source.onerror = function () {
    appendLine("event-warn", "connection interrupted, retrying…");
  };
})();
