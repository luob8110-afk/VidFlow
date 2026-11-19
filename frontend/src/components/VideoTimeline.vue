<script setup lang="ts">
import { computed, onScopeDispose, ref, watch } from 'vue'

const props = defineProps<{ video?: HTMLVideoElement | null }>()

const duration = ref(0)
const position = ref(0)
const pointerTime = ref<number | null>(null)
const dragPointer = ref<number | null>(null)
const pointerFocused = ref(false)
const slider = ref<HTMLInputElement | null>(null)
let dragListeners: AbortController | undefined

const canSeek = computed(() => duration.value > 0)
const playhead = computed(() => dragPointer.value === null ? position.value : (pointerTime.value ?? position.value))
const labelTime = computed(() => pointerTime.value ?? playhead.value)

function clampTime(time: number) {
  return Number.isFinite(time) ? Math.min(duration.value, Math.max(0, time)) : 0
}

function reset() {
  duration.value = 0
  position.value = 0
  cancelDrag()
}

function endDrag() {
  dragListeners?.abort()
  dragListeners = undefined
  dragPointer.value = null
}

function cancelDrag() {
  endDrag()
  pointerTime.value = null
}

onScopeDispose(endDrag)

watch(() => props.video, (video, _, onCleanup) => {
  reset()
  if (!video) return

  function sync() {
    if (!video || video.readyState < 1 || video.error || !Number.isFinite(video.duration) || video.duration <= 0) {
      reset()
      return
    }
    duration.value = video.duration
    position.value = clampTime(video.currentTime)
  }

  const listener = new AbortController()
  for (const event of ['loadedmetadata', 'durationchange', 'timeupdate', 'seeking', 'seeked']) {
    video.addEventListener(event, sync, { signal: listener.signal })
  }
  video.addEventListener('emptied', reset, { signal: listener.signal })
  video.addEventListener('error', reset, { signal: listener.signal })
  onCleanup(() => listener.abort())
  sync()
}, { immediate: true })

function timeText(seconds: number) {
  const total = Math.floor(seconds)
  const hours = Math.floor(total / 3600)
  const minutes = String(Math.floor(total / 60) % 60).padStart(2, '0')
  const remainder = String(total % 60).padStart(2, '0')
  return hours > 0 ? `${hours}:${minutes}:${remainder}` : `${minutes}:${remainder}`
}

function percentage(time: number) {
  return canSeek.value ? `${clampTime(time) / duration.value * 100}%` : '0%'
}

function pointerTimeAt(event: PointerEvent, bounds: DOMRect) {
  return clampTime((event.clientX - bounds.left) / bounds.width * duration.value)
}

function seek(time: number) {
  const video = props.video
  if (!video || !canSeek.value) return
  video.currentTime = clampTime(time)
  position.value = clampTime(video.currentTime)
}

function onSliderInput(event: Event) {
  if (dragPointer.value !== null) return
  pointerTime.value = null
  seek((event.currentTarget as HTMLInputElement).valueAsNumber)
}

function movePointer(event: PointerEvent) {
  if (!canSeek.value || dragPointer.value !== null || event.pointerType === 'touch') return
  pointerTime.value = pointerTimeAt(event, (event.currentTarget as HTMLElement).getBoundingClientRect())
}

function startDrag(event: PointerEvent) {
  if (!canSeek.value || event.button !== 0 || dragPointer.value !== null) return
  const timeline = event.currentTarget as HTMLElement
  event.preventDefault()
  pointerFocused.value = true
  slider.value?.focus({ preventScroll: true })
  dragPointer.value = event.pointerId
  pointerTime.value = pointerTimeAt(event, timeline.getBoundingClientRect())
  dragListeners = new AbortController()
  const options = { signal: dragListeners.signal }
  window.addEventListener('pointermove', (move) => {
    if (move.pointerId !== dragPointer.value) return
    if (move.pointerType === 'mouse' && move.buttons === 0) {
      cancelDrag()
      return
    }
    pointerTime.value = pointerTimeAt(move, timeline.getBoundingClientRect())
  }, options)
  window.addEventListener('pointerup', (up) => {
    if (up.pointerId !== dragPointer.value) return
    const bounds = timeline.getBoundingClientRect()
    const time = pointerTimeAt(up, bounds)
    const stillHovering = up.pointerType !== 'touch' && up.clientX >= bounds.left && up.clientX <= bounds.right &&
      up.clientY >= bounds.top && up.clientY <= bounds.bottom
    endDrag()
    pointerTime.value = stillHovering ? time : null
    seek(time)
  }, options)
  window.addEventListener('pointercancel', (cancel) => {
    if (cancel.pointerId === dragPointer.value) cancelDrag()
  }, options)
  window.addEventListener('blur', cancelDrag, options)
}

function leavePointer() {
  if (dragPointer.value === null) pointerTime.value = null
}
</script>

<template>
  <div
    class="video-timeline"
    :class="{ 'is-expanded': canSeek && (pointerTime !== null || dragPointer !== null), 'pointer-focused': pointerFocused }"
    :style="{ '--played': percentage(playhead), '--label-at': percentage(labelTime) }"
    @click.stop @dblclick.stop
    @pointerenter="movePointer" @pointermove="movePointer" @pointerleave="leavePointer"
    @pointerdown="startDrag"
  >
    <div class="timeline-track" aria-hidden="true">
      <span class="timeline-fill" />
      <span class="timeline-thumb" />
    </div>
    <span v-if="canSeek" class="timeline-time" aria-hidden="true">{{ timeText(labelTime) }}</span>
    <input
      ref="slider" class="timeline-slider" type="range" min="0" :max="duration || 1" step="any" :value="playhead"
      :disabled="!canSeek" aria-label="播放进度" :aria-valuetext="`${timeText(playhead)} / ${timeText(duration)}`"
      @input="onSliderInput"
      @keydown="pointerFocused = false" @blur="pointerFocused = false"
    />
  </div>
</template>

<style scoped>
.video-timeline {
  --timeline-thickness: 3px;
  --timeline-thumb-opacity: 0;
  --timeline-time-visibility: hidden;
  position: absolute;
  z-index: 1;
  inset-inline: 14px;
  bottom: 0;
  height: 28px;
  cursor: pointer;
  touch-action: none;
}

.video-timeline.is-expanded,
.video-timeline:not(.pointer-focused):has(.timeline-slider:focus-visible) {
  --timeline-thickness: 7px;
  --timeline-thumb-opacity: 1;
  --timeline-time-visibility: visible;
}

.timeline-track {
  position: absolute;
  inset-inline: 0;
  top: 50%;
  height: var(--timeline-thickness);
  transform: translateY(-50%);
  border-radius: 999px;
  background: rgba(255, 255, 255, 0.34);
  box-shadow: 0 1px 5px rgba(0, 0, 0, 0.4);
  transition: height 140ms ease;
  pointer-events: none;
}

.timeline-fill {
  display: block;
  width: var(--played);
  height: 100%;
  border-radius: inherit;
  background: var(--accent, #8de4d0);
}

.timeline-thumb {
  position: absolute;
  top: 50%;
  left: var(--played);
  width: 11px;
  height: 11px;
  transform: translate(-50%, -50%);
  border-radius: 50%;
  background: #effff9;
  opacity: var(--timeline-thumb-opacity);
  transition: opacity 140ms ease;
}

.timeline-time {
  position: absolute;
  bottom: calc(50% + 8px);
  left: clamp(35px, var(--label-at), calc(100% - 35px));
  transform: translateX(-50%);
  padding: 3px 7px;
  border-radius: 6px;
  background: rgba(13, 19, 27, 0.92);
  color: #f3f7fa;
  font: 12px/1.35 ui-monospace, monospace;
  visibility: var(--timeline-time-visibility);
  pointer-events: none;
}

.timeline-slider {
  position: absolute;
  inset: 0;
  width: 100%;
  height: 100%;
  margin: 0;
  padding: 0;
  opacity: 0;
  pointer-events: none;
}

.video-timeline:not(.pointer-focused):has(.timeline-slider:focus-visible) .timeline-track {
  outline: 1px solid #f3f7fa;
  outline-offset: 3px;
}
.video-timeline:has(.timeline-slider:disabled) { pointer-events: none; opacity: 0.45; }

@media (max-width: 620px) {
  .video-timeline { inset-inline: 8px; }
}

@media (prefers-reduced-motion: reduce) {
  .timeline-track, .timeline-thumb { transition: none; }
}
</style>
