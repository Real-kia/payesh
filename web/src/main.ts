import { mount } from 'svelte';
import './styles/theme.css';
import './styles/ui.css';
import App from './App.svelte';

mount(App, { target: document.getElementById('app')! });
