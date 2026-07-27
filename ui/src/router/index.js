import { createRouter, createWebHistory } from 'vue-router'
import Dashboard from '@/views/Dashboard.vue'

const router = createRouter({
  history: createWebHistory(import.meta.env.BASE_URL),
  routes: [
    // Selection lives in the URL so a turn can be linked to and reloaded.
    { path: '/', name: 'dashboard', component: Dashboard },
    { path: '/s/:session/:id?', name: 'turn', component: Dashboard },
  ],
})

export default router
