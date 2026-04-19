import { micCapture } from './src/core/audio/MicCapture.js';
import { speakerOutput } from './src/core/audio/SpeakerOutput.js';
import { eventBus } from './src/core/agent/EventBus.js';
import { setupFfmpeg } from './src/utils/audio.js';

async function testAudio() {
    console.log('Setting up binaries...');
    await setupFfmpeg();
    
    console.log('Listening for audio levels on EventBus...');
    eventBus.onEvent('audio:level', (event) => {
        const bar = '█'.repeat(Math.floor(event.level * 20));
        process.stdout.write(`\r${event.source.toUpperCase()}: ${bar.padEnd(20)} ${(event.level * 100).toFixed(1)}%   `);
    });

    console.log('\nRecording and playing back in 3 seconds...');
    const stream = micCapture.start();
    
    stream.on('data', (chunk) => {
        // Echo back with slight delay or just direct pipe
        speakerOutput.addChunk(chunk);
    });

    setTimeout(() => {
        console.log('\nStopping test...');
        micCapture.stop();
        speakerOutput.stop();
        process.exit(0);
    }, 5000);
}

testAudio().catch(console.error);
