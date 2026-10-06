---
name: Bug report
about: Something doesn't work on your router
labels: bug
---

**What happened**

<!-- What you did, what you expected, what you got instead. -->

**Router**

- Model:
- RouterOS version:
- Image (arm64 / armv7 / amd64) and release:

**Output**

<!-- Remove IP addresses and keys you don't want to share. -->

```
/system/resource/print
/container/print detail
/log/print where topics~"container"
/routing/rule/print
/ip/route/print where routing-table=to-awg
```

**Web UI logs**

```
(paste the "Logs" section here)
```
