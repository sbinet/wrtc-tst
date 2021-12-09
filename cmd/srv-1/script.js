"use strict";

let conn = new WebSocket('wss://' + window.location.host + '/ws')
let pc = new RTCPeerConnection()

var btnStart = document.getElementById("start-btn");
var btnStop = document.getElementById("stop-btn");

var log = msg => {
    var el = document.getElementById('logs')
    el.innerHTML = msg + '<br>' + el.innerHTML
}

navigator.mediaDevices.getDisplayMedia({
        video: true,
        audio: false
    })
    .then(stream => {
        stream.getTracks().forEach(track => pc.addTrack(track, stream))
        document.getElementById('video1').srcObject = stream
        pc.createOffer().then(d => pc.setLocalDescription(d)).catch(log)
    }).catch(log)


window.startClick = () => {
    conn.send(JSON.stringify({
        name: 'start',
        data: ''
    }))
    btnStart.disabled = true;
    btnStop.disabled = false;
}

window.stopClick = () => {
    log("stopping...")
    var video = document.getElementById("video1");
    var stream = video.srcObject;
    if (stream != null && stream.active) {
        let trks = stream.getTracks();
        trks.forEach(trk => trk.stop());
        video.srcObject = null;
    }

    conn.send(JSON.stringify({
        name: 'stop',
        data: ''
    }))
    log("stopping... [done]")

    btnStart.disabled = false;
    btnStop.disabled = true;
}


// pc.ontrack = function(event) {
//     if (event.track.kind === 'audio') {
//         return
//     }
//     var el = document.getElementById('video1')
//     el.srcObject = event.streams[0]
//     el.autoplay = true
//     el.controls = true
// }

conn.onopen = () => {
    pc.createOffer({
        offerToReceiveVideo: true,
        offerToReceiveAudio: false
    }).then(offer => {
        pc.setLocalDescription(offer)
        conn.send(JSON.stringify({
            name: 'offer',
            data: JSON.stringify(offer)
        }))
    })
}

conn.onclose = evt => {
    console.log('Connection closed')
}

conn.onmessage = evt => {
    let msg = JSON.parse(evt.data)
    if (!msg) {
        return console.log('failed to parse msg')
    }
    switch (msg.name) {
        case 'answer':
            var answer = JSON.parse(msg.data)
            if (!answer) {
                return console.log('failed to parse answer')
            }
            pc.setRemoteDescription(answer)
    }
}

window.conn = conn
